package relayserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/fqix/podrelay/internal/relayclient"
)

// webApp stands in for VS Code in the pod: it reports what it received and
// echoes WebSocket messages.
func webApp(t *testing.T) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			typ, msg, err := c.Read(r.Context())
			if err == nil {
				c.Write(r.Context(), typ, msg)
			}
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"path": r.URL.Path, "query": r.URL.RawQuery, "host": r.Host, "cookie": r.Header.Get("Cookie"),
		})
	}))
	t.Cleanup(srv.Close)
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ = strconv.Atoi(p)
	return h, port
}

type webFixture struct {
	srv      *Server
	relay    *httptest.Server
	key      ssh.Signer
	appPort  int
	wsURL    string
	browser  *http.Client
	redirect []string
}

func newWebFixture(t *testing.T) *webFixture {
	t.Helper()
	host, port := webApp(t)
	key := newSigner(t)
	pods := fakeResolver{
		"ns/web": {keys: string(ssh.MarshalAuthorizedKey(key.PublicKey())), addr: net.JoinHostPort(host, "22")},
	}
	f := &webFixture{
		srv:     &Server{Pods: pods, Logger: slog.New(slog.DiscardHandler), SessionKey: []byte("test-key")},
		key:     key,
		appPort: port,
	}
	f.relay = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.relay.Close)
	f.wsURL = "ws" + strings.TrimPrefix(f.relay.URL, "http")
	jar, _ := cookiejar.New(nil)
	f.browser = &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		f.redirect = append(f.redirect, r.URL.RequestURI())
		return nil
	}}
	return f
}

func (f *webFixture) ticket(t *testing.T, key ssh.Signer, target string, port int) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return relayclient.Ticket(ctx, relayclient.Options{URL: f.wsURL, Signer: key}, target, port)
}

func (f *webFixture) get(t *testing.T, path string) (int, map[string]string) {
	t.Helper()
	resp, err := f.browser.Get(f.relay.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]string
	json.Unmarshal(body, &got)
	return resp.StatusCode, got
}

func TestTicketOpensSessionAndProxies(t *testing.T) {
	f := newWebFixture(t)
	tk, err := f.ticket(t, f.key, "ns/web", f.appPort)
	if err != nil {
		t.Fatal(err)
	}

	code, got := f.get(t, "/vscode/ns/web/?tkn=abc&ticket="+url.QueryEscape(tk))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	// The ticket is swapped for a cookie and removed from the URL.
	if len(f.redirect) != 1 || strings.Contains(f.redirect[0], "ticket") || !strings.Contains(f.redirect[0], "tkn=abc") {
		t.Fatalf("redirects = %q", f.redirect)
	}
	if got["path"] != "/vscode/ns/web/" || got["query"] != "tkn=abc" {
		t.Errorf("app saw %s?%s", got["path"], got["query"])
	}
	if strings.Contains(got["cookie"], sessionCookie) {
		t.Error("the relay's session cookie leaked to the app")
	}
	if got["host"] != strings.TrimPrefix(f.relay.URL, "http://") {
		t.Errorf("app saw Host %q, want the relay's", got["host"])
	}

	// Later requests ride on the cookie, including WebSockets.
	if code, got := f.get(t, "/vscode/ns/web/static/app.js"); code != http.StatusOK || got["path"] != "/vscode/ns/web/static/app.js" {
		t.Errorf("follow-up request: %d %v", code, got)
	}
	u, _ := url.Parse(f.relay.URL + "/vscode/ns/web/")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, f.wsURL+"/vscode/ns/web/socket", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {f.browser.Jar.Cookies(u)[0].String()}},
	})
	if err != nil {
		t.Fatalf("websocket through the proxy: %v", err)
	}
	defer c.CloseNow()
	c.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, msg, err := c.Read(ctx); err != nil || string(msg) != "hi" {
		t.Fatalf("websocket echo: %q %v", msg, err)
	}
}

func TestWebRequiresSession(t *testing.T) {
	f := newWebFixture(t)
	if code, _ := f.get(t, "/vscode/ns/web/"); code != http.StatusUnauthorized {
		t.Errorf("no cookie: status %d, want 401", code)
	}

	tk, _ := f.ticket(t, f.key, "ns/web", f.appPort)
	f.get(t, "/vscode/ns/web/?ticket="+url.QueryEscape(tk))
	u, _ := url.Parse(f.relay.URL + "/vscode/ns/web/")
	session := f.browser.Jar.Cookies(u)[0].Value

	try := func(path, cookie string) int {
		req, _ := http.NewRequest(http.MethodGet, f.relay.URL+path, nil)
		req.Header.Set("Cookie", sessionCookie+"="+cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := try("/vscode/ns/other/", session); code != http.StatusUnauthorized {
		t.Errorf("session for ns/web used on ns/other: status %d, want 401", code)
	}
	if code := try("/vscode/ns/web/", session[:len(session)-2]+"xx"); code != http.StatusUnauthorized {
		t.Errorf("tampered cookie: status %d, want 401", code)
	}
	if code := try("/vscode/ns/web/?ticket="+url.QueryEscape(session), ""); code != http.StatusForbidden {
		t.Errorf("session used as a ticket: status %d, want 403", code)
	}
	if code := try("/vscode/ns/web/", tk); code != http.StatusUnauthorized {
		t.Errorf("ticket used as a session: status %d, want 401", code)
	}

	f.srv.now = func() time.Time { return time.Now().Add(13 * time.Hour) }
	if code := try("/vscode/ns/web/", session); code != http.StatusUnauthorized {
		t.Errorf("expired session: status %d, want 401", code)
	}
}

func TestTicketRules(t *testing.T) {
	f := newWebFixture(t)
	if _, err := f.ticket(t, newSigner(t), "ns/web", f.appPort); err == nil || err.Error() != "access denied" {
		t.Errorf("unauthorized key: %v", err)
	}
	if _, err := f.ticket(t, f.key, "ns/web", 0); err == nil || !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("port 0: %v", err)
	}

	tk, err := f.ticket(t, f.key, "ns/web", f.appPort)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.get(t, "/vscode/ns/other/?ticket="+url.QueryEscape(tk)); code != http.StatusForbidden {
		t.Errorf("ticket for ns/web on ns/other: status %d, want 403", code)
	}
	f.srv.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	if code, _ := f.get(t, "/vscode/ns/web/?ticket="+url.QueryEscape(tk)); code != http.StatusForbidden {
		t.Errorf("expired ticket: status %d, want 403", code)
	}
}

func TestVSCodePathWithoutTrailingSlashRedirects(t *testing.T) {
	f := newWebFixture(t)
	f.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := f.browser.Get(f.relay.URL + "/vscode/ns/web?ticket=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "/vscode/ns/web/?ticket=x" {
		t.Errorf("got %d Location %q", resp.StatusCode, loc)
	}
}
