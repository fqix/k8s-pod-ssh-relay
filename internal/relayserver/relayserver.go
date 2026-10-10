// Package relayserver is the relay's HTTP side: it authenticates clients
// with their SSH key against the target pod's authorized keys (see
// internal/relayproto) and pipes the WebSocket to the pod's sshd.
//
//	GET /healthz                        liveness/readiness
//	GET /connect?target=namespace/pod   WebSocket; auth handshake, then the sshd stream
package relayserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/fqix/k8s-pod-ssh-relay/internal/netutil"
	"github.com/fqix/k8s-pod-ssh-relay/internal/relayproto"
	"github.com/fqix/k8s-pod-ssh-relay/internal/target"
	"github.com/fqix/k8s-pod-ssh-relay/internal/wsconn"
)

// Resolver looks up pods; *podresolver.Resolver implements it.
type Resolver interface {
	// AuthorizedKeys returns the pod's keys in authorized_keys format.
	AuthorizedKeys(namespace, pod string) (string, error)
	// Resolve returns the host:port of the pod's sshd.
	Resolve(namespace, pod string) (string, error)
}

type Server struct {
	Pods Resolver
	// HandshakeTimeout bounds authentication (default 10s).
	HandshakeTimeout time.Duration
	// DialTimeout bounds connecting to the pod's sshd (default 5s).
	DialTimeout time.Duration
	// Logger logs connection events (default slog.Default()).
	Logger *slog.Logger
}

// Handler serves /healthz and /connect.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/connect", s.connect)
	return mux
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("target")
	from := clientAddr(r)
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	hctx, cancel := context.WithTimeout(r.Context(), or(s.HandshakeTimeout, 10*time.Second))
	defer cancel()
	// The client signed the raw target string, so authenticate against it.
	pub, err := relayproto.Authenticate(hctx, c, raw, func(string) ([]ssh.PublicKey, error) {
		t, err := target.ParseCanonical(raw)
		if err != nil {
			return nil, err
		}
		keys, err := s.Pods.AuthorizedKeys(t.Namespace, t.Pod)
		if err != nil {
			return nil, err
		}
		return relayproto.ParseAuthorizedKeys(keys)
	})
	if err != nil {
		// Unknown pod and unauthorized key look the same to the client.
		s.log().Warn("auth failed", "from", from, "target", raw, "err", err)
		relayproto.SendResult(hctx, c, errors.New("access denied"))
		c.Close(websocket.StatusPolicyViolation, "unauthorized")
		return
	}
	who := ssh.FingerprintSHA256(pub)
	t, _ := target.ParseCanonical(raw) // validated during authentication

	sshd, err := s.Pods.Resolve(t.Namespace, t.Pod)
	var backend net.Conn
	if err == nil {
		backend, err = net.DialTimeout("tcp", sshd, or(s.DialTimeout, 5*time.Second))
	}
	if err != nil {
		s.log().Warn("connect failed", "from", from, "key", who, "target", t.String(), "err", err)
		relayproto.SendResult(hctx, c, err)
		c.Close(websocket.StatusNormalClosure, "")
		return
	}
	if err := relayproto.SendResult(hctx, c, nil); err != nil {
		backend.Close()
		return
	}
	log := s.log().With("from", from, "key", who, "target", t.String(), "sshd", sshd)
	log.Info("session opened")
	start := time.Now()
	netutil.Pipe(wsconn.NetConn(r.Context(), c), backend)
	log.Info("session closed", "duration", time.Since(start).Round(time.Second))
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// clientAddr is the client's address as seen by the ingress controller. The
// Service is ClusterIP-only, so X-Real-IP can only come from inside the cluster.
func clientAddr(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
