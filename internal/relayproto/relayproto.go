// Package relayproto defines the k8s-pod-ssh-relay wire protocol, which
// authenticates a client with its SSH key before the relay connects it to a
// pod's sshd. Both sides are implemented here; vscode/src/relay.ts is a second
// client implementation and must follow this description.
//
// # Connection
//
// The client opens a WebSocket to
//
//	GET /connect?target=<namespace>/<pod>
//
// The target is in canonical form (see internal/target) and is exactly the
// string that gets signed.
//
// # Handshake
//
// Three JSON text messages follow the upgrade:
//
//	relay  -> client  {"nonce": "<base64, 32 random bytes>"}
//	client -> relay   {"public_key": "<base64 SSH wire-format public key>",
//	                   "signature": {"Format": "<algorithm>", "Blob": "<base64>"}}
//	relay  -> client  {"ok": true}  or  {"error": "<message>"}
//
// The signature is an SSH signature (as produced by ssh.Signer, or by
// ssh-agent) over Message(nonce, target): the SSH wire encoding of
//
//	string  "k8s-pod-ssh-relay-auth-v1"
//	string  nonce
//	string  target
//
// where each string is a uint32 big-endian length followed by the bytes. RSA
// keys sign with rsa-sha2-256.
//
// The relay accepts the client if the public key is listed in the target
// pod's authorized keys (decided per target by a KeySource) and the signature
// verifies. A fresh nonce per connection prevents replay, and signing the
// target binds the signature to that pod. An unknown pod and an unauthorized
// key produce the same error, so clients can't probe which pods exist.
//
// # Stream
//
// After {"ok": true}, binary messages carry the pod's sshd byte stream in
// both directions until either side closes the WebSocket.
package relayproto

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/ssh"
)

type Challenge struct {
	Nonce []byte `json:"nonce"`
}

type Response struct {
	PublicKey []byte         `json:"public_key"` // SSH wire format
	Signature *ssh.Signature `json:"signature"`
}

type Result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Message is the data the client signs.
func Message(nonce []byte, target string) []byte {
	return ssh.Marshal(struct {
		Magic  string
		Nonce  []byte
		Target string
	}{"k8s-pod-ssh-relay-auth-v1", nonce, target})
}

// KeySource returns the public keys allowed to reach target.
type KeySource func(target string) ([]ssh.PublicKey, error)

// Authenticate runs the server side of the handshake and returns the
// client's verified public key. Errors are detailed and meant for the server
// log; send clients a generic denial so they can't probe which pods exist.
func Authenticate(ctx context.Context, c *websocket.Conn, target string, keys KeySource) (ssh.PublicKey, error) {
	nonce := make([]byte, 32)
	rand.Read(nonce)
	if err := wsjson.Write(ctx, c, Challenge{Nonce: nonce}); err != nil {
		return nil, err
	}
	var resp Response
	if err := wsjson.Read(ctx, c, &resp); err != nil {
		return nil, err
	}
	pub, err := ssh.ParsePublicKey(resp.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("bad public key: %w", err)
	}
	allowed, err := keys(target)
	if err != nil {
		return nil, err
	}
	if !contains(allowed, pub) {
		return nil, fmt.Errorf("public key %s not authorized for %s", ssh.FingerprintSHA256(pub), target)
	}
	if resp.Signature == nil {
		return nil, errors.New("missing signature")
	}
	if err := pub.Verify(Message(nonce, target), resp.Signature); err != nil {
		return nil, errors.New("signature verification failed")
	}
	return pub, nil
}

// SendResult tells the client whether it may proceed.
func SendResult(ctx context.Context, c *websocket.Conn, err error) error {
	if err != nil {
		return wsjson.Write(ctx, c, Result{Error: err.Error()})
	}
	return wsjson.Write(ctx, c, Result{OK: true})
}

// Login runs the client side of the handshake.
func Login(ctx context.Context, c *websocket.Conn, target string, signer ssh.Signer) error {
	var ch Challenge
	if err := wsjson.Read(ctx, c, &ch); err != nil {
		return err
	}
	sig, err := sign(signer, Message(ch.Nonce, target))
	if err != nil {
		return err
	}
	if err := wsjson.Write(ctx, c, Response{PublicKey: signer.PublicKey().Marshal(), Signature: sig}); err != nil {
		return err
	}
	var res Result
	if err := wsjson.Read(ctx, c, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	return nil
}

func sign(s ssh.Signer, data []byte) (*ssh.Signature, error) {
	// Plain "ssh-rsa" signatures use SHA-1; prefer SHA-256 for RSA keys.
	if as, ok := s.(ssh.AlgorithmSigner); ok && s.PublicKey().Type() == ssh.KeyAlgoRSA {
		return as.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA256)
	}
	return s.Sign(rand.Reader, data)
}

// ParseAuthorizedKeys parses keys in authorized_keys format, one per line;
// blank lines and comments are skipped.
func ParseAuthorizedKeys(data string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	rest := []byte(data)
	for len(bytes.TrimSpace(rest)) > 0 {
		key, _, _, r, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
		rest = r
	}
	return keys, nil
}

func contains(keys []ssh.PublicKey, pub ssh.PublicKey) bool {
	want := pub.Marshal()
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), want) {
			return true
		}
	}
	return false
}
