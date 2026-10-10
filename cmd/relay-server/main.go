// relay-server bridges websocket clients (ssh ProxyCommand) to the native
// sshd of a pod. A client names a pod and proves it holds one of the keys in
// that pod's k8s-pod-ssh-relay/authorized-keys annotation (see internal/auth); the
// relay then looks up the pod's address in an informer cache and dials it.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/fqix/k8s-pod-ssh-relay/internal/auth"
	"github.com/fqix/k8s-pod-ssh-relay/internal/k8s"
	"github.com/fqix/k8s-pod-ssh-relay/internal/wsconn"
)

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	namespace := flag.String("namespace", "k8s-pod-ssh-relay", "namespace of target pods")
	sshPort := flag.Int("ssh-port", 22, "sshd port when a pod has no "+k8s.AnnotationPort+" annotation")
	flag.Parse()
	pods, err := k8s.NewResolver(context.Background(), *namespace, *sshPort)
	if err != nil {
		log.Fatalf("pod informer: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// Clients dial in here with ?target=<pod name>.
	mux.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		from := clientAddr(r)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		hctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		pub, err := auth.Authenticate(hctx, c, target, func(target string) ([]ssh.PublicKey, error) {
			keys, err := pods.AuthorizedKeys(target)
			if err != nil {
				return nil, err
			}
			return auth.ParseAuthorizedKeys(keys)
		})
		if err != nil {
			// Unknown pod and unauthorized key look the same to the client.
			log.Printf("%s -> %s: auth failed: %v", from, target, err)
			auth.SendResult(hctx, c, errors.New("access denied"))
			c.Close(websocket.StatusPolicyViolation, "unauthorized")
			return
		}
		who := ssh.FingerprintSHA256(pub)

		sshd, err := pods.Resolve(target)
		var backend net.Conn
		if err == nil {
			backend, err = net.DialTimeout("tcp", sshd, 5*time.Second)
		}
		if err != nil {
			log.Printf("%s [%s] -> %s: %v", from, who, target, err)
			auth.SendResult(hctx, c, err)
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if err := auth.SendResult(hctx, c, nil); err != nil {
			backend.Close()
			return
		}
		log.Printf("%s [%s] -> %s (%s): session opened", from, who, target, sshd)
		wsconn.Pipe(wsconn.NetConn(r.Context(), c), backend)
		log.Printf("%s [%s] -> %s (%s): session closed", from, who, target, sshd)
	})

	log.Printf("relay-server listening on %s (namespace=%s annotation=%s=true)", *addr, *namespace, k8s.AnnotationEnabled)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// clientAddr is the client's address as seen by the ingress controller. The
// Service is ClusterIP-only, so X-Real-IP can only come from inside the cluster.
func clientAddr(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
