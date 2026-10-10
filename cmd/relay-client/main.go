// relay-client is meant to be used as an ssh ProxyCommand: it connects to the
// relay, authenticates with an SSH key, and pipes stdin/stdout to the sshd of
// the target pod.
//
//	ssh -o ProxyCommand='relay-client -relay wss://HOST -i ~/.ssh/id_ed25519 -target %h' demo@<pod-name>
//
// Port forwarding (ssh -L/-R/-D) runs inside the SSH connection, so it needs
// nothing from the relay.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/fqix/k8s-pod-ssh-relay/internal/auth"
	"github.com/fqix/k8s-pod-ssh-relay/internal/wsconn"
)

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { os.Stdin.Close(); return os.Stdout.Close() }

func main() {
	home, _ := os.UserHomeDir()
	relay := flag.String("relay", os.Getenv("RELAY_URL"), "relay base URL (defaults to $RELAY_URL)")
	target := flag.String("target", "", "target pod name; a trailing .relay suffix is stripped")
	caFile := flag.String("ca", "", "PEM CA bundle to trust for wss:// in addition to the system roots")
	connectTo := flag.String("connect-to", "", "dial this host:port instead of resolving the relay URL's host (Host header and TLS name are unchanged)")
	identity := flag.String("i", filepath.Join(home, ".ssh", "id_ed25519"),
		"private key; if it is passphrase-protected, the matching key in ssh-agent is used")
	flag.Parse()
	if *relay == "" || *target == "" {
		fmt.Fprintln(os.Stderr, "usage: relay-client -relay wss://host [-ca ca.crt] [-connect-to ip:port] [-i key] -target <pod-name>")
		os.Exit(2)
	}
	id := strings.TrimSuffix(*target, ".relay")

	signer, err := loadSigner(*identity)
	if err != nil {
		fatal(err)
	}

	httpClient, err := newHTTPClient(*caFile, *connectTo)
	if err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, resp, err := websocket.Dial(ctx, *relay+"/connect?target="+url.QueryEscape(id), &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		fatal(err)
	}
	if err := auth.Login(ctx, c, id, signer); err != nil {
		fatal(err)
	}
	cancel()
	wsconn.Pipe(wsconn.NetConn(context.Background(), c), stdio{})
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "relay-client: %v\n", err)
	os.Exit(1)
}

// newHTTPClient trusts caFile (if set) on top of the system roots, e.g. for a
// self-signed ingress certificate, and dials connectTo (if set) instead of the
// URL's host, like curl --connect-to.
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

// loadSigner reads an unencrypted private key directly. For a
// passphrase-protected key (we can't prompt: stdin belongs to ssh) it uses the
// key with the same public half from ssh-agent.
func loadSigner(path string) (ssh.Signer, error) {
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
