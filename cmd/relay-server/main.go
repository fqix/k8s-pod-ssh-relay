// relay-server bridges websocket clients (ssh ProxyCommand, relay-client web,
// the VS Code extension) to the native sshd of a pod. A client names a pod as
// namespace/pod and proves it holds one of the keys in that pod's
// k8s-pod-ssh-relay/authorized-keys annotation; the relay then looks up the
// pod's address in an informer cache and dials it. See internal/relayserver.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/fqix/k8s-pod-ssh-relay/internal/podresolver"
	"github.com/fqix/k8s-pod-ssh-relay/internal/relayserver"
)

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	namespaces := flag.String("namespaces", "", "comma-separated namespaces to watch for target pods (default: all)")
	sshPort := flag.Int("ssh-port", 22, "sshd port when a pod has no "+podresolver.AnnotationPort+" annotation")
	flag.Parse()

	var watch []string
	for _, ns := range strings.Split(*namespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			watch = append(watch, ns)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pods, err := podresolver.NewResolver(context.Background(), watch, *sshPort, logger)
	if err != nil {
		logger.Error("pod informer", "err", err)
		os.Exit(1)
	}

	scope := "all"
	if len(watch) > 0 {
		scope = strings.Join(watch, ",")
	}
	logger.Info("relay-server listening", "addr", *addr, "namespaces", scope, "annotation", podresolver.AnnotationEnabled+"=true")
	err = http.ListenAndServe(*addr, (&relayserver.Server{Pods: pods, Logger: logger}).Handler())
	logger.Error("listen", "err", err)
	os.Exit(1)
}
