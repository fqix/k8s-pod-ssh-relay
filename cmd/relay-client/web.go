package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/fqix/k8s-pod-ssh-relay/internal/netutil"
	"github.com/fqix/k8s-pod-ssh-relay/internal/podssh"
	"github.com/fqix/k8s-pod-ssh-relay/internal/target"
	"github.com/fqix/k8s-pod-ssh-relay/internal/vscodeserver"
)

// runWeb starts VS Code for the Web in a pod and opens it in the browser:
//
//  1. SSH into the pod through the relay
//  2. install and start the VS Code server's web build inside the pod
//  3. forward a local port to it over SSH
//  4. wait until the workbench is actually served, then open the browser
func runWeb(args []string) {
	fs := flag.NewFlagSet("relay-client web", flag.ExitOnError)
	flags := addRelayFlags(fs)
	user := fs.String("user", "root", "login user inside the pod")
	port := fs.Int("port", 0, "local port (default: derived from the target, so each pod keeps the same browser origin)")
	folder := fs.String("folder", "", "folder to open in the pod (default: the user's home)")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	commit := fs.String("commit", "", "VS Code commit to run (default: latest stable)")
	quality := fs.String("quality", "stable", "VS Code quality: stable or insider")
	downloadURL := fs.String("download-url", vscodeserver.DefaultDownloadURL,
		"server download URL; variables: ${commit} ${quality} ${platform} ${os} ${arch}")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: relay-client web [flags] namespace/pod")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	t, err := target.Parse(fs.Arg(0))
	if err != nil {
		fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pod := podssh.New(flags.options(), t.String(), *user, logger)
	defer pod.Close()
	step("connecting to " + t.String())
	srv, err := vscodeserver.Ensure(pod, vscodeserver.Options{
		Commit:      *commit,
		Quality:     *quality,
		DownloadURL: *downloadURL,
		Step:        step,
		Progress:    os.Stderr,
	})
	if err != nil {
		fatal(err)
	}

	ln, err := listenLocal(*port, t.String())
	if err != nil {
		fatal(err)
	}
	defer ln.Close()
	go forward(ln, pod, net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Port)), logger)

	base := "http://" + ln.Addr().String()
	step("waiting for VS Code to be ready")
	if err := waitReady(base, srv.Token, 2*time.Minute); err != nil {
		fatal(err)
	}
	if *folder == "" {
		*folder = srv.Home
	}
	open := base + "/?" + url.Values{"tkn": {srv.Token}, "folder": {*folder}}.Encode()
	fmt.Fprintf(os.Stderr, "\nVS Code for the Web (%s): %s\n\n", t, open)
	if !*noBrowser {
		if err := openBrowser(open); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser (%v); open the URL above\n", err)
		}
	}
	fmt.Fprintln(os.Stderr, "Press Ctrl+C to stop. The server in the pod shuts itself down once idle.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func step(msg string) {
	fmt.Fprintf(os.Stderr, "==> %s\n", msg)
}

// listenLocal listens on 127.0.0.1. Without an explicit port it uses one
// derived from the target, so the browser origin (and with it VS Code's
// per-origin UI state) is stable for each pod; it falls back to a random
// port if that one is taken.
func listenLocal(port int, target string) (net.Listener, error) {
	if port != 0 {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	h := fnv.New32a()
	h.Write([]byte(target))
	if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+int(h.Sum32()%10000)))); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// forward pipes each local connection to addr inside the pod.
func forward(ln net.Listener, pod *podssh.Client, addr string, logger *slog.Logger) {
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			remote, err := pod.Dial(addr)
			if err != nil {
				logger.Warn("forward failed", "addr", addr, "err", err)
				local.Close()
				return
			}
			netutil.Pipe(local, remote)
		}()
	}
}

// waitReady polls the workbench page through the tunnel until the server
// serves it, so the browser never opens onto a server that is still starting.
func waitReady(base, token string, timeout time.Duration) error {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := hc.Get(base + "/?tkn=" + url.QueryEscape(token))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("HTTP %s", resp.Status)
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("VS Code did not become ready within %s: %v", timeout, last)
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
