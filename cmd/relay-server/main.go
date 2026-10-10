// relay-server bridges websocket clients (ssh ProxyCommand, relay-client web,
// the VS Code extension) to the native sshd of a pod. A client names a pod as
// namespace/pod and proves it holds one of the keys in that pod's
// k8s-pod-ssh-relay/authorized-keys annotation; the relay then looks up the
// pod's address in an informer cache and dials it. See internal/relayserver.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/fqix/k8s-pod-ssh-relay/internal/podresolver"
	"github.com/fqix/k8s-pod-ssh-relay/internal/relayserver"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	namespaces := flag.String("namespaces", "", "comma-separated namespaces to watch for target pods (default: all)")
	sshPort := flag.Int("ssh-port", 22, "sshd port when a pod has no "+podresolver.AnnotationPort+" annotation")
	sessionKeyFile := flag.String("session-key-file", "", "secret that signs browser tickets and sessions; all replicas must share it")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	var watch []string
	for _, ns := range strings.Split(*namespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			watch = append(watch, ns)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var sessionKey []byte
	if *sessionKeyFile != "" {
		key, err := os.ReadFile(*sessionKeyFile)
		if err != nil || len(bytes.TrimSpace(key)) < 16 {
			logger.Error("session key", "file", *sessionKeyFile, "err", err, "hint", "need at least 16 bytes")
			os.Exit(1)
		}
		sessionKey = bytes.TrimSpace(key)
	}
	pods, err := podresolver.NewResolver(context.Background(), watch, *sshPort, logger)
	if err != nil {
		logger.Error("pod informer", "err", err)
		os.Exit(1)
	}

	scope := "all"
	if len(watch) > 0 {
		scope = strings.Join(watch, ",")
	}
	logger.Info("relay-server listening", "version", version, "addr", *addr, "namespaces", scope, "annotation", podresolver.AnnotationEnabled+"=true")
	err = http.ListenAndServe(*addr, (&relayserver.Server{Pods: pods, Logger: logger, SessionKey: sessionKey}).Handler())
	logger.Error("listen", "err", err)
	os.Exit(1)
}
