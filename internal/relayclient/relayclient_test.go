package relayclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// writeKey writes an OpenSSH private key (encrypted if passphrase != "") and
// its .pub, and returns the key's path and public key.
func writeKey(t *testing.T, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644)
	return path, sshPub
}

func TestLoadSignerPlainKey(t *testing.T) {
	path, pub := writeKey(t, "")
	s, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("loaded the wrong key")
	}
}

func TestLoadSignerEncryptedKeyNeedsAgent(t *testing.T) {
	path, _ := writeKey(t, "secret")
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := LoadSigner(path); err == nil || !strings.Contains(err.Error(), "no ssh-agent") {
		t.Fatalf("error = %v, want no ssh-agent", err)
	}
}

func TestLoadSignerEncryptedKeyFromAgent(t *testing.T) {
	path, pub := writeKey(t, "secret")
	sock := startAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	if _, err := LoadSigner(path); err == nil || !strings.Contains(err.Error(), "ssh-add") {
		t.Fatalf("key not in agent: error = %v, want a hint to ssh-add", err)
	}

	// Load the same key into the agent (decrypting it as ssh-add would).
	raw, _ := os.ReadFile(path)
	priv, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := net.Dial("unix", sock)
	defer c.Close()
	if err := agent.NewClient(c).Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("agent returned the wrong key")
	}
	if _, err := s.Sign(rand.Reader, []byte("data")); err != nil {
		t.Fatalf("sign through agent: %v", err)
	}
}

// startAgent serves an in-memory ssh-agent on a unix socket.
func startAgent(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; t.TempDir can be longer on macOS.
	dir, err := os.MkdirTemp("/tmp", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	keyring := agent.NewKeyring()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, c)
		}
	}()
	return sock
}
