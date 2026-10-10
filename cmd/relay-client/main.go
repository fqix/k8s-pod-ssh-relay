// relay-client connects to pods through k8s-pod-ssh-relay.
//
// Without a subcommand it is an ssh ProxyCommand: it connects to the relay,
// authenticates with an SSH key, and pipes stdin/stdout to the sshd of the
// target pod.
//
//	ssh -o ProxyCommand='relay-client -relay wss://HOST -i ~/.ssh/id_ed25519 -target %h' demo@<pod>.<namespace>.relay
//
// Port forwarding (ssh -L/-R/-D) runs inside the SSH connection, so it needs
// nothing from the relay.
//
// "relay-client web namespace/pod" starts VS Code for the Web in the pod and
// opens it in the local browser (see web.go).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fqix/k8s-pod-ssh-relay/internal/netutil"
	"github.com/fqix/k8s-pod-ssh-relay/internal/relayclient"
	"github.com/fqix/k8s-pod-ssh-relay/internal/target"
)

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { os.Stdin.Close(); return os.Stdout.Close() }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "web" {
		runWeb(os.Args[2:])
		return
	}
	runProxy(os.Args[1:])
}

func runProxy(args []string) {
	fs := flag.NewFlagSet("relay-client", flag.ExitOnError)
	flags := addRelayFlags(fs)
	targetFlag := fs.String("target", "", "target pod: namespace/pod, or the ssh host form <pod>.<namespace>[.relay]")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: relay-client -relay wss://host [-connect-to ip:port] [-i key] -target namespace/pod")
		fmt.Fprintln(os.Stderr, "       relay-client web [flags] namespace/pod   (VS Code for the Web; see relay-client web -h)")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *targetFlag == "" {
		fs.Usage()
		os.Exit(2)
	}
	t, err := target.Parse(*targetFlag)
	if err != nil {
		fatal(err)
	}
	conn, err := relayclient.Dial(context.Background(), flags.options(), t.String())
	if err != nil {
		fatal(err)
	}
	netutil.Pipe(conn, stdio{})
}

// relayFlags are the flags shared by all modes.
type relayFlags struct {
	url, connectTo, identity string
}

func addRelayFlags(fs *flag.FlagSet) *relayFlags {
	home, _ := os.UserHomeDir()
	f := &relayFlags{}
	fs.StringVar(&f.url, "relay", os.Getenv("RELAY_URL"), "relay base URL: wss://host, or ws://host for local testing (defaults to $RELAY_URL)")
	fs.StringVar(&f.connectTo, "connect-to", "", "dial this host:port instead of resolving the relay URL's host (Host header and TLS name are unchanged)")
	fs.StringVar(&f.identity, "i", filepath.Join(home, ".ssh", "id_ed25519"),
		"private key; if it is passphrase-protected, the matching key in ssh-agent is used")
	return f
}

// options validates the flags and loads the key; it exits on error.
func (f *relayFlags) options() relayclient.Options {
	if f.url == "" {
		fatal(fmt.Errorf("-relay (or $RELAY_URL) is required"))
	}
	signer, err := relayclient.LoadSigner(f.identity)
	if err != nil {
		fatal(err)
	}
	return relayclient.Options{URL: f.url, ConnectTo: f.connectTo, Signer: signer}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "relay-client: %v\n", err)
	os.Exit(1)
}
