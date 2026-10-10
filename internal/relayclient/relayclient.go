// Package relayclient connects to a pod's sshd through podrelay:
// it dials the relay over WebSocket, authenticates with an SSH key (see
// internal/relayproto), and returns the byte stream to the pod's sshd.
package relayclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/fqix/podrelay/internal/relayproto"
	"github.com/fqix/podrelay/internal/wsconn"
)

// Options says how to reach and authenticate to the relay.
type Options struct {
	// URL is the relay base URL: wss://relay.example.com in production (TLS
	// verified against the system roots), or ws:// for local testing.
	URL string
	// ConnectTo, if set, is dialed instead of the URL's host; the Host header
	// and TLS name are unchanged (like curl --connect-to).
	ConnectTo string
	// Signer authenticates to the relay.
	Signer ssh.Signer
}

// Dial connects to the relay, authenticates for target ("namespace/pod"),
// and returns the stream to the pod's sshd.
func Dial(ctx context.Context, o Options, target string) (net.Conn, error) {
	c, err := dialWS(ctx, o, "/connect", url.Values{"target": {target}})
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := relayproto.Login(hctx, c, target, o.Signer); err != nil {
		c.CloseNow()
		return nil, err
	}
	return wsconn.NetConn(context.Background(), c), nil
}

// Ticket authenticates for target and returns a short-lived ticket that
// opens a browser session to the pod's port (see the relayproto docs).
func Ticket(ctx context.Context, o Options, target string, port int) (string, error) {
	c, err := dialWS(ctx, o, "/ticket", url.Values{"target": {target}, "port": {strconv.Itoa(port)}})
	if err != nil {
		return "", err
	}
	defer c.CloseNow()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return relayproto.LoginTicket(hctx, c, target, o.Signer)
}

// WebURL turns the relay URL into its https:// (or http://) form.
func WebURL(o Options) string {
	u := strings.TrimRight(o.URL, "/")
	if rest, ok := strings.CutPrefix(u, "wss://"); ok {
		return "https://" + rest
	}
	if rest, ok := strings.CutPrefix(u, "ws://"); ok {
		return "http://" + rest
	}
	return u
}

// HTTPClient returns a client that honors ConnectTo, for plain HTTP
// requests to the relay.
func HTTPClient(o Options) *http.Client {
	return newHTTPClient(o.ConnectTo)
}

func dialWS(ctx context.Context, o Options, path string, q url.Values) (*websocket.Conn, error) {
	httpClient := newHTTPClient(o.ConnectTo)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(hctx, strings.TrimRight(o.URL, "/")+path+"?"+q.Encode(),
		&websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, err
	}
	return c, nil
}

// LoadSigner reads an unencrypted private key directly. For a
// passphrase-protected key (we can't prompt: in ProxyCommand mode stdin
// belongs to ssh) it uses the key with the same public half from ssh-agent.
func LoadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	want := missing.PublicKey
	if want == nil {
		pubData, err := os.ReadFile(path + ".pub")
		if err != nil {
			return nil, fmt.Errorf("%s is encrypted and %s.pub is unreadable: %w", path, path, err)
		}
		if want, _, _, _, err = ssh.ParseAuthorizedKey(pubData); err != nil {
			return nil, err
		}
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("%s is passphrase-protected and no ssh-agent is running", path)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	for _, s := range signers {
		if bytes.Equal(s.PublicKey().Marshal(), want.Marshal()) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%s is passphrase-protected and not loaded in ssh-agent (run: ssh-add %s)", path, path)
}

// newHTTPClient dials connectTo (if set) instead of the URL's host, like
// curl --connect-to; the Host header and TLS server name are unchanged.
func newHTTPClient(connectTo string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if connectTo != "" {
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, connectTo)
		}
		tr.Proxy = nil
	}
	return &http.Client{Transport: tr}
}
