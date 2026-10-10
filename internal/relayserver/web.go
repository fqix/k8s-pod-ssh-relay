package relayserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/fqix/podrelay/internal/relayproto"
	"github.com/fqix/podrelay/internal/target"
)

// Browser access to a web app in a pod (VS Code for the Web):
//
//  1. podrelay authenticates on /ticket with its SSH key, exactly like
//     /connect, and gets a short-lived ticket for namespace/pod and a port.
//  2. The browser opens /vscode/<ns>/<pod>/?ticket=...; the relay turns the
//     ticket into a session cookie scoped to that path and redirects.
//  3. Every request under /vscode/<ns>/<pod>/ needs a valid session cookie and
//     is reverse-proxied (WebSockets included) to podIP:port, path unchanged.
//
// Tickets and sessions are HMAC-signed with SessionKey, which all replicas
// must share. The web app keeps its own auth (VS Code's connection token) on
// top.
const (
	vscodePrefix  = "/vscode/"
	sessionCookie = "podrelay-session"
)

// claims are what a ticket or session cookie grants.
type claims struct {
	Kind   string `json:"k"` // "ticket" or "session"
	Target string `json:"t"` // namespace/pod
	Port   int    `json:"p"`
	Key    string `json:"f"` // SSH key fingerprint, for logs
	Exp    int64  `json:"e"` // unix seconds
}

func (s *Server) ticket(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("target")
	from := clientAddr(r)
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	hctx, cancel := context.WithTimeout(r.Context(), or(s.HandshakeTimeout, 10*time.Second))
	defer cancel()

	pub, err := relayproto.Authenticate(hctx, c, raw, s.keysFor(raw))
	if err != nil {
		s.log().Warn("ticket auth failed", "from", from, "target", raw, "err", err)
		relayproto.SendResult(hctx, c, errors.New("access denied"))
		return
	}
	// Checked after the handshake so the result is always the third message.
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		relayproto.SendResult(hctx, c, errors.New("invalid port"))
		return
	}
	who := ssh.FingerprintSHA256(pub)
	tok := s.sign(claims{Kind: "ticket", Target: raw, Port: port, Key: who, Exp: s.clock().Add(or(s.TicketTTL, 2*time.Minute)).Unix()})
	if err := relayproto.SendTicket(hctx, c, tok); err == nil {
		s.log().Info("ticket issued", "from", from, "key", who, "target", raw, "port", port)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) vscode(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, vscodePrefix), "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	t, err := target.ParseCanonical(parts[0] + "/" + parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base := vscodePrefix + t.Namespace + "/" + t.Pod + "/"
	if len(parts) == 2 { // /vscode/ns/pod -> /vscode/ns/pod/
		http.Redirect(w, r, base+query(r.URL.Query()), http.StatusFound)
		return
	}

	if tk := r.URL.Query().Get("ticket"); tk != "" {
		c, err := s.verify(tk, "ticket")
		if err != nil || c.Target != t.String() {
			s.denied(w, http.StatusForbidden, "This link is invalid or has expired. Run podrelay web again to get a new one.")
			return
		}
		c.Kind = "session"
		c.Exp = s.clock().Add(or(s.SessionTTL, 12*time.Hour)).Unix()
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    s.sign(c),
			Path:     base,
			MaxAge:   int(or(s.SessionTTL, 12*time.Hour) / time.Second),
			HttpOnly: true,
			Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			SameSite: http.SameSiteLaxMode,
		})
		s.log().Info("web session started", "from", clientAddr(r), "key", c.Key, "target", c.Target, "port", c.Port)
		q := r.URL.Query()
		q.Del("ticket")
		http.Redirect(w, r, r.URL.Path+query(q), http.StatusSeeOther)
		return
	}

	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		s.denied(w, http.StatusUnauthorized, "No session for this pod. Open it with podrelay web.")
		return
	}
	c, err := s.verify(ck.Value, "session")
	if err != nil || c.Target != t.String() {
		s.denied(w, http.StatusUnauthorized, "Your session for this pod has expired. Open it again with podrelay web.")
		return
	}
	ip, err := s.Pods.PodIP(t.Namespace, t.Pod)
	if err != nil {
		s.log().Warn("web proxy", "target", t.String(), "err", err)
		s.denied(w, http.StatusBadGateway, "The pod is not reachable.")
		return
	}
	backend := net.JoinHostPort(ip, strconv.Itoa(c.Port))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: backend})
			pr.Out.Host = pr.In.Host // the app checks Origin against Host
			stripCookie(pr.Out, sessionCookie)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log().Warn("web proxy", "target", t.String(), "backend", backend, "err", err)
			s.denied(w, http.StatusBadGateway, "The web app in the pod is not responding. Start it again with podrelay web.")
		},
	}
	proxy.ServeHTTP(w, r)
}

// keysFor returns the KeySource for a raw target string.
func (s *Server) keysFor(raw string) relayproto.KeySource {
	return func(string) ([]ssh.PublicKey, error) {
		t, err := target.ParseCanonical(raw)
		if err != nil {
			return nil, err
		}
		keys, err := s.Pods.AuthorizedKeys(t.Namespace, t.Pod)
		if err != nil {
			return nil, err
		}
		return relayproto.ParseAuthorizedKeys(keys)
	}
}

func (s *Server) sign(c claims) string {
	payload, _ := json.Marshal(c)
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(s.mac(payload))
}

func (s *Server) verify(tok, kind string) (claims, error) {
	p, sig, ok := strings.Cut(tok, ".")
	payload, err1 := b64.DecodeString(p)
	mac, err2 := b64.DecodeString(sig)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(mac, s.mac(payload)) {
		return claims{}, errors.New("bad signature")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return claims{}, err
	}
	if c.Kind != kind {
		return claims{}, fmt.Errorf("not a %s", kind)
	}
	if s.clock().Unix() >= c.Exp {
		return claims{}, errors.New("expired")
	}
	return c, nil
}

func (s *Server) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.sessionKey())
	m.Write(payload)
	return m.Sum(nil)
}

func (s *Server) sessionKey() []byte {
	s.keyOnce.Do(func() {
		if len(s.SessionKey) == 0 {
			s.SessionKey = make([]byte, 32)
			rand.Read(s.SessionKey)
			s.log().Warn("no session key configured; using a random one, so browser sessions won't work across replicas or restarts")
		}
	})
	return s.SessionKey
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) denied(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, "<!doctype html><title>Remote - PodRelay</title><p>%s</p>\n", html.EscapeString(msg))
}

func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

func query(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

var b64 = base64.RawURLEncoding
