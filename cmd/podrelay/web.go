package main

import (
	"context"
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
	"strings"
	"syscall"
	"time"

	"github.com/fqix/podrelay/internal/netutil"
	"github.com/fqix/podrelay/internal/podssh"
	"github.com/fqix/podrelay/internal/relayclient"
	"github.com/fqix/podrelay/internal/target"
	"github.com/fqix/podrelay/internal/vscodeserver"
)

// runWeb starts VS Code for the Web in a pod and opens it in the browser.
//
// By default the page is served through the relay's Ingress:
//
//  1. SSH into the pod through the relay; install and start the VS Code
//     server's web build on the pod IP, under /vscode/<namespace>/<pod>/
//  2. authenticate to the relay with the SSH key and get a short-lived ticket
//  3. check through the Ingress that the workbench is served, then open
//     https://<relay>/vscode/<namespace>/<pod>/?ticket=...; the relay swaps the
//     ticket for a session cookie and proxies to the pod. The command exits.
//
// With -local the server stays on 127.0.0.1 in the pod and a local port is
// forwarded to it over SSH instead; the command runs until interrupted.
func runWeb(args []string) {
	fs := flag.NewFlagSet("podrelay web", flag.ExitOnError)
	flags := addRelayFlags(fs)
	user := fs.String("user", "root", "login user inside the pod")
	local := fs.Bool("local", false, "serve on a local port forwarded over SSH instead of through the relay's Ingress")
	podPort := fs.Int("pod-port", 13337, "port VS Code listens on inside the pod (Ingress mode)")
	port := fs.Int("port", 0, "local port with -local (default: derived from the target, so each pod keeps the same browser origin)")
	folder := fs.String("folder", "", "folder to open in the pod (default: the user's home)")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	commit := fs.String("commit", "", "VS Code commit to run (default: latest stable)")
	quality := fs.String("quality", "stable", "VS Code quality: stable or insider")
	downloadURL := fs.String("download-url", vscodeserver.DefaultDownloadURL,
		"server download URL; variables: ${commit} ${quality} ${platform} ${os} ${arch}")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: podrelay web [flags] namespace/pod")
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

	opts := flags.options()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pod := podssh.New(opts, t.String(), *user, logger)
	defer pod.Close()

	so := vscodeserver.Options{
		Commit:      *commit,
		Quality:     *quality,
		DownloadURL: *downloadURL,
		Step:        step,
		Progress:    os.Stderr,
	}
	basePath := "/vscode/" + t.Namespace + "/" + t.Pod + "/"
	if !*local {
		so.Host, so.Port, so.BasePath = "0.0.0.0", *podPort, strings.TrimSuffix(basePath, "/")
	}
	step("connecting to " + t.String())
	srv, err := vscodeserver.Ensure(pod, so)
	if err != nil {
		fatal(err)
	}
	if *folder == "" {
		*folder = srv.Home
	}

	var open string
	if *local {
		ln, err := listenLocal(*port, t.String())
		if err != nil {
			fatal(err)
		}
		defer ln.Close()
		go forward(ln, pod, net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Port)), logger)
		base := "http://" + ln.Addr().String() + "/"
		step("waiting for VS Code to be ready")
		if err := waitReady(&http.Client{Transport: &http.Transport{Proxy: nil}}, base+"?tkn="+url.QueryEscape(srv.Token), 2*time.Minute); err != nil {
			fatal(err)
		}
		open = base + "?" + url.Values{"tkn": {srv.Token}, "folder": {*folder}}.Encode()
	} else {
		pod.Close() // the browser goes through the relay from here on
		base := relayclient.WebURL(opts) + basePath
		hc := relayclient.HTTPClient(opts)
		step("waiting for VS Code to be ready behind " + relayclient.WebURL(opts))
		err = waitReadyFunc(func() (string, error) {
			tk, err := relayclient.Ticket(context.Background(), opts, t.String(), srv.Port)
			return base + "?" + url.Values{"ticket": {tk}, "tkn": {srv.Token}}.Encode(), err
		}, hc, 2*time.Minute)
		if err != nil {
			fatal(err)
		}
		// A fresh ticket for the browser; it is good for a couple of minutes.
		tk, err := relayclient.Ticket(context.Background(), opts, t.String(), srv.Port)
		if err != nil {
			fatal(err)
		}
		open = base + "?" + url.Values{"ticket": {tk}, "tkn": {srv.Token}, "folder": {*folder}}.Encode()
	}

	fmt.Fprintf(os.Stderr, "\nVS Code for the Web (%s):\n  %s\n\n", t, open)
	if !*noBrowser {
		if err := openBrowser(open); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser (%v); open the URL above\n", err)
		}
	}
	if !*local {
		if opts.ConnectTo != "" {
			fmt.Fprintf(os.Stderr, "Note: the browser must itself resolve %s to the Ingress; -connect-to only applies to podrelay.\n", relayclient.WebURL(opts))
		}
		fmt.Fprintln(os.Stderr, "The link expires in 2 minutes; the browser session it opens lasts 12 hours.")
		return
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

// waitReady polls url until it serves 200 (following redirects with a
// cookie jar), so the browser never opens onto a server that is still
// starting.
func waitReady(hc *http.Client, url string, timeout time.Duration) error {
	return waitReadyFunc(func() (string, error) { return url, nil }, hc, timeout)
}

// waitReadyFunc is waitReady with a URL computed per attempt (e.g. with a
// fresh ticket).
func waitReadyFunc(next func() (string, error), hc *http.Client, timeout time.Duration) error {
	jar, _ := cookiejar.New(nil)
	c := *hc
	c.Jar = jar
	c.Timeout = 10 * time.Second
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		u, err := next()
		if err == nil {
			var resp *http.Response
			if resp, err = c.Get(u); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
				err = fmt.Errorf("HTTP %s", resp.Status)
			}
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
