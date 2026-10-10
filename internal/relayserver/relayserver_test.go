package relayserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fqix/k8s-pod-ssh-relay/internal/relayclient"
)

type fakePod struct {
	keys    string
	addr    string
	resolve error
}

type fakeResolver map[string]fakePod

func (f fakeResolver) AuthorizedKeys(ns, pod string) (string, error) {
	p, ok := f[ns+"/"+pod]
	if !ok {
		return "", errors.New("pod " + ns + "/" + pod + " not found")
	}
	return p.keys, nil
}

func (f fakeResolver) Resolve(ns, pod string) (string, error) {
	p := f[ns+"/"+pod]
	return p.addr, p.resolve
}

func (f fakeResolver) PodIP(ns, pod string) (string, error) {
	p := f[ns+"/"+pod]
	if p.resolve != nil {
		return "", p.resolve
	}
	host, _, err := net.SplitHostPort(p.addr)
	return host, err
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// echoServer stands in for a pod's sshd.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func startRelay(t *testing.T, pods fakeResolver) string {
	t.Helper()
	srv := httptest.NewServer((&Server{Pods: pods, Logger: slog.New(slog.DiscardHandler)}).Handler())
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(url string, signer ssh.Signer, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return relayclient.Dial(ctx, relayclient.Options{URL: url, Signer: signer}, target)
}

func TestConnectPipesToPod(t *testing.T) {
	key := newSigner(t)
	url := startRelay(t, fakeResolver{
		"ns/web": {keys: string(ssh.MarshalAuthorizedKey(key.PublicKey())), addr: echoServer(t)},
	})
	conn, err := dial(url, key, "ns/web")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := strings.Repeat("ping ", 20000) // spans many WebSocket messages
	go conn.Write([]byte(msg))
	got := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatal("echoed bytes differ")
	}
}

func TestDeniedLooksTheSame(t *testing.T) {
	key, other := newSigner(t), newSigner(t)
	url := startRelay(t, fakeResolver{
		"ns/web": {keys: string(ssh.MarshalAuthorizedKey(other.PublicKey())), addr: echoServer(t)},
	})
	for _, target := range []string{
		"ns/web",     // key not authorized for this pod
		"ns/missing", // pod doesn't exist
		"web.ns",     // not the canonical form
		"ns/Bad_Name",
	} {
		_, err := dial(url, key, target)
		if err == nil || err.Error() != "access denied" {
			t.Errorf("target %q: error = %v, want exactly \"access denied\"", target, err)
		}
	}
}

func TestResolveErrorReportedAfterAuth(t *testing.T) {
	key := newSigner(t)
	url := startRelay(t, fakeResolver{
		"ns/web": {keys: string(ssh.MarshalAuthorizedKey(key.PublicKey())), resolve: errors.New("pod ns/web is not running")},
	})
	if _, err := dial(url, key, "ns/web"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error = %v, want the resolve error", err)
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{Pods: fakeResolver{}}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestClientAddrPrefersXRealIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/connect", nil)
	r.RemoteAddr = "10.0.0.9:1234"
	if got := clientAddr(r); got != "10.0.0.9:1234" {
		t.Errorf("clientAddr = %q", got)
	}
	r.Header.Set("X-Real-IP", "192.168.64.1")
	if got := clientAddr(r); got != "192.168.64.1" {
		t.Errorf("clientAddr with X-Real-IP = %q", got)
	}
}
