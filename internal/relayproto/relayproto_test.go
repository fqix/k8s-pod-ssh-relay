package relayproto

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// relay starts a server that authenticates every connection for target
// against keys and reports the result on the returned channel.
func relay(t *testing.T, target string, keys ...ssh.PublicKey) (string, <-chan error) {
	t.Helper()
	results := make(chan error, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, err = Authenticate(r.Context(), c, target, func(string) ([]ssh.PublicKey, error) { return keys, nil })
		SendResult(r.Context(), c, err)
		results <- err
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), results
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func TestLoginAuthorized(t *testing.T) {
	key := newSigner(t)
	url, results := relay(t, "ns/pod", newSigner(t).PublicKey(), key.PublicKey())
	if err := Login(context.Background(), dial(t, url), "ns/pod", key); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := <-results; err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestLoginUnauthorizedKey(t *testing.T) {
	url, results := relay(t, "ns/pod", newSigner(t).PublicKey())
	if err := Login(context.Background(), dial(t, url), "ns/pod", newSigner(t)); err == nil {
		t.Fatal("Login succeeded with an unauthorized key")
	}
	if err := <-results; err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("Authenticate error = %v, want not authorized", err)
	}
}

func TestSignatureBoundToTarget(t *testing.T) {
	key := newSigner(t)
	url, results := relay(t, "ns/other", key.PublicKey())
	// The client signs for ns/pod but the relay checks ns/other.
	if err := Login(context.Background(), dial(t, url), "ns/pod", key); err == nil {
		t.Fatal("Login succeeded with a signature for another target")
	}
	if err := <-results; err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("Authenticate error = %v, want signature failure", err)
	}
}

func TestReplayedResponseRejected(t *testing.T) {
	ctx := context.Background()
	key := newSigner(t)
	url, results := relay(t, "ns/pod", key.PublicKey())

	// First connection: answer the challenge by hand and keep the response.
	c := dial(t, url)
	var ch Challenge
	if err := wsjson.Read(ctx, c, &ch); err != nil {
		t.Fatal(err)
	}
	sig, err := key.Sign(rand.Reader, Message(ch.Nonce, "ns/pod"))
	if err != nil {
		t.Fatal(err)
	}
	resp := Response{PublicKey: key.PublicKey().Marshal(), Signature: sig}
	if err := wsjson.Write(ctx, c, resp); err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}

	// Second connection: send the same response to a fresh nonce.
	c2 := dial(t, url)
	if err := wsjson.Read(ctx, c2, &ch); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, c2, resp); err != nil {
		t.Fatal(err)
	}
	if err := <-results; err == nil {
		t.Fatal("replayed response was accepted")
	}
}

// The TypeScript client (vscode/src/relay.ts) builds the same bytes.
func TestMessageWireFormat(t *testing.T) {
	got := hex.EncodeToString(Message([]byte("abc"), "ns/p"))
	want := "00000019" + hex.EncodeToString([]byte("k8s-pod-ssh-relay-auth-v1")) +
		"00000003" + hex.EncodeToString([]byte("abc")) +
		"00000004" + hex.EncodeToString([]byte("ns/p"))
	if got != want {
		t.Fatalf("Message = %s, want %s", got, want)
	}
}

func TestParseAuthorizedKeys(t *testing.T) {
	a, b := newSigner(t).PublicKey(), newSigner(t).PublicKey()
	data := "# comment\n\n" + string(ssh.MarshalAuthorizedKey(a)) + "  \n" +
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(b))) + " user@host\n"
	keys, err := ParseAuthorizedKeys(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !contains(keys, a) || !contains(keys, b) {
		t.Fatalf("got %d keys", len(keys))
	}
	if keys, err := ParseAuthorizedKeys(""); err != nil || len(keys) != 0 {
		t.Fatalf("empty: %v, %v", keys, err)
	}
	if _, err := ParseAuthorizedKeys("not a key\n"); err == nil {
		t.Fatal("accepted garbage")
	}
}
