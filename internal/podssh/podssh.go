// Package podssh keeps an SSH client to a pod over the relay: it sends
// keepalives and redials through the relay when the connection drops (e.g.
// the relay restarted).
package podssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fqix/k8s-pod-ssh-relay/internal/relayclient"
)

// Client is a reconnecting SSH client to one pod.
type Client struct {
	log    *slog.Logger
	relay  relayclient.Options
	target string
	config *ssh.ClientConfig

	mu     sync.Mutex
	client *ssh.Client
}

// New returns a client for target ("namespace/pod") that logs in as user
// with the relay's signer. It connects lazily. A nil logger means
// slog.Default().
func New(relay relayclient.Options, target, user string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{log: logger, relay: relay, target: target, config: &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(relay.Signer)},
		// Pods generate new host keys on every start; the relay already
		// authenticated the target by name over TLS.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}}
}

// Connect returns the current SSH client, dialing through the relay if needed.
func (c *Client) Connect() (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	nc, err := relayclient.Dial(context.Background(), c.relay, c.target)
	if err != nil {
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(nc, c.target, c.config)
	if err != nil {
		nc.Close()
		return nil, err
	}
	cl := ssh.NewClient(sc, chans, reqs)
	c.client = cl
	go c.watch(cl)
	return cl, nil
}

// Dial opens a direct-tcpip channel to addr as seen from inside the pod,
// redialing once if the current connection turns out to be dead.
func (c *Client) Dial(addr string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		cl, err := c.Connect()
		if err != nil {
			return nil, err
		}
		conn, err := cl.Dial("tcp", addr)
		if err == nil || attempt == 1 {
			return conn, err
		}
		c.drop(cl)
	}
}

// Run runs a POSIX sh script in the pod with positional args ($1, $2, ...),
// feeding stdin if given, and returns its stdout.
func (c *Client) Run(script string, stdin io.Reader, args ...string) (string, error) {
	cl, err := c.Connect()
	if err != nil {
		return "", err
	}
	s, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	var stdout, stderr bytes.Buffer
	s.Stdin, s.Stdout, s.Stderr = stdin, &stdout, &stderr
	cmd := []string{shQuote("sh"), shQuote("-c"), shQuote(script), shQuote("sh")}
	for _, a := range args {
		cmd = append(cmd, shQuote(a))
	}
	if err := s.Run(strings.Join(cmd, " ")); err != nil {
		return "", fmt.Errorf("remote script: %v: %s", err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.String(), nil
}

// Close closes the current connection.
func (c *Client) Close() {
	c.mu.Lock()
	cl := c.client
	c.client = nil
	c.mu.Unlock()
	if cl != nil {
		cl.Close()
	}
}

func (c *Client) watch(cl *ssh.Client) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, _, err := cl.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					cl.Close()
					return
				}
			}
		}
	}()
	cl.Wait()
	close(done)
	if c.drop(cl) {
		c.log.Warn("ssh connection lost; reconnecting on the next request", "target", c.target)
	}
}

// drop forgets cl if it is current; reports whether it was.
func (c *Client) drop(cl *ssh.Client) bool {
	c.mu.Lock()
	current := c.client == cl
	if current {
		c.client = nil
	}
	c.mu.Unlock()
	cl.Close()
	return current
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
