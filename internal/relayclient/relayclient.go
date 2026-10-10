// Package relayclient connects to a pod's sshd through k8s-pod-ssh-relay:
// it dials the relay over WebSocket, authenticates with an SSH key (see
// internal/relayproto), and returns the byte stream to the pod's sshd.
package relayclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/fqix/k8s-pod-ssh-relay/internal/relayproto"
	"github.com/fqix/k8s-pod-ssh-relay/internal/wsconn"
)

// Options says how to reach and authenticate to the relay.
type Options struct {
	// URL is the relay base URL, e.g. wss://relay.example.com.
	URL string
	// CAFile is an extra PEM CA bundle to trust, e.g. for a self-signed Ingress.
	CAFile string
	// ConnectTo, if set, is dialed instead of the URL's host; the Host header
	// and TLS name are unchanged (like curl --connect-to).
	ConnectTo string
	// Signer authenticates to the relay.
	Signer ssh.Signer
}

// Dial connects to the relay, authenticates for target ("namespace/pod"),
// and returns the stream to the pod's sshd.
func Dial(ctx context.Context, o Options, target string) (net.Conn, error) {
	httpClient, err := newHTTPClient(o.CAFile, o.ConnectTo)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(hctx, strings.TrimRight(o.URL, "/")+"/connect?target="+url.QueryEscape(target),
		&websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, err
	}
	if err := relayproto.Login(hctx, c, target, o.Signer); err != nil {
		c.CloseNow()
		return nil, err
	}
	return wsconn.NetConn(context.Background(), c), nil
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

func newHTTPClient(caFile, connectTo string) (*http.Client, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{}}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates found", caFile)
		}
		tr.TLSClientConfig.RootCAs = roots
	}
	if connectTo != "" {
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, connectTo)
		}
	}
	return &http.Client{Transport: tr}, nil
}
