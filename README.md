# k8s-pod-ssh-relay

English | [简体中文](README.zh-CN.md)

SSH into Kubernetes pods that run a stock `sshd`, through a WebSocket relay used as the ssh `ProxyCommand`. Target pods expose no Service and no port outside the cluster, and need no agent or sidecar.

```
ssh ──stdio──> relay-client ──wss──> Ingress (TLS) ──ws──> relay-server ──TCP──> <podIP>:22 (sshd)
```

## Features

- **Plain ssh**: works with `ssh`, `scp`, `sftp`, `rsync -e ssh`, VS Code Remote-SSH, and port forwarding (`-L` / `-R` / `-D`).
- **Address pods by name**: `ssh <pod-name>.relay`. The relay resolves the name from a pod informer cache, with no API call per connection.
- **Opt in per pod with annotations**: only pods annotated `k8s-pod-ssh-relay/ssh: "true"` are reachable.
- **SSH key authentication, per pod**: the client proves it holds a key listed in the target pod's `k8s-pod-ssh-relay/authorized-keys` annotation. sshd reads the same annotation, so it is the single source of keys. Changes take effect without restarts.
- **TLS** at an ingress-nginx Ingress (`wss://`). Only `/connect` is exposed.

## How it works

1. `ssh` starts `relay-client` as its `ProxyCommand` and speaks SSH over its stdin/stdout.
2. `relay-client` opens `wss://<host>/connect?target=<pod>` through the Ingress.
3. **Authentication.** The relay sends a random 32-byte nonce. The client signs `(nonce, target pod)` with its private key. The relay verifies the signature against the target pod's `authorized-keys` annotation.
   - The nonce prevents replay, and including the pod name binds the signature to that pod.
   - An unknown pod and an unauthorized key both return the same `access denied`, so clients can't probe which pods exist. The relay log records the real reason and the key fingerprint.
4. The relay looks up the pod's IP in its informer cache and dials the pod's sshd directly. From then on it copies bytes in both directions.
5. SSH runs end to end between your `ssh` and the pod's `sshd`, so the relay only ever sees encrypted SSH traffic.

### Pod annotations

| Annotation | Required | Meaning |
|---|---|---|
| `k8s-pod-ssh-relay/ssh` | yes | Must be `"true"` for the pod to be reachable |
| `k8s-pod-ssh-relay/authorized-keys` | yes | Allowed public keys, in `authorized_keys` format, one per line |
| `k8s-pod-ssh-relay/ssh-port` | no | sshd port (default `22`) |

The target image is plain Alpine with OpenSSH. sshd reads `authorized-keys` from a downward API volume through `AuthorizedKeysCommand`. It can't use `AuthorizedKeysFile` because the downward API mount directory has mode `1777`, which sshd's `StrictModes` check rejects.

## Quick start (minikube)

Requirements: minikube, kubectl, Go, OpenSSL.

```bash
minikube addons enable ingress
make all
```

`make all` does the following:
- builds the binaries
- builds the images inside minikube
- generates an SSH key and a self-signed CA and TLS certificate under `keys/`
- deploys everything to the `k8s-pod-ssh-relay` namespace
- writes an `ssh_config`

```bash
make pods                                # list pods and their annotations
make ssh                                 # log into the first target pod
ssh -F ssh_config <pod-name>.relay
scp -F ssh_config file <pod-name>.relay:/tmp/
```

The generated `ssh_config` matches `Host *.relay` and runs:

```
relay-client -relay wss://k8s-pod-ssh-relay.test -ca keys/ca.crt -connect-to <minikube-ip>:443 -i keys/id_ed25519 -target %h
```

To use it without `-F`, add `Include /path/to/ssh_config` at the top of `~/.ssh/config`.

### Grant or revoke access on a live pod

```bash
kubectl -n k8s-pod-ssh-relay annotate --overwrite pod <pod> \
  k8s-pod-ssh-relay/authorized-keys="$(cat keys/id_ed25519.pub ~/.ssh/other.pub)"
```

- The relay picks up the change immediately through the informer.
- sshd picks it up once the kubelet syncs the downward API file, which can take up to about a minute.

### Port forwarding

Forwarding runs as channels inside the SSH connection, so it needs nothing from the relay:

```bash
ssh -F ssh_config -N -L 2224:kubernetes.default.svc:443 <pod-name>.relay  # local 2224 -> anything the pod can reach
ssh -F ssh_config -N -R 9000:127.0.0.1:3000 <pod-name>.relay              # pod's 127.0.0.1:9000 -> local 3000
ssh -F ssh_config -N -D 1080 <pod-name>.relay                             # SOCKS5 proxy egressing from the pod
```

## relay-client flags

| Flag | Description |
|---|---|
| `-relay` | Relay base URL, e.g. `wss://relay.example.com` (default `$RELAY_URL`) |
| `-target` | Pod name. A trailing `.relay` is stripped, so `%h` can be passed directly |
| `-i` | Private key (default `~/.ssh/id_ed25519`). If it is passphrase-protected, the matching key in `ssh-agent` is used, because the client can't prompt while ssh owns its stdin |
| `-ca` | Extra PEM CA to trust, e.g. for a self-signed Ingress certificate |
| `-connect-to` | Dial this `host:port` instead of resolving the URL's host. The Host header and TLS verification still use the URL's host, like curl's `--connect-to` |

With real DNS for the Ingress host and a publicly trusted certificate, both `-connect-to` and `-ca` can be dropped.

## Security notes

- Anyone who can patch pod annotations in the namespace can grant themselves SSH access. Restrict `patch` on pods with RBAC, or add an admission policy for the `k8s-pod-ssh-relay/*` annotations.
- The relay needs only `list`/`watch` on pods in its namespace.
- The included NetworkPolicy allows port 22 on target pods only from the relay. It is enforced only by a CNI that supports NetworkPolicy, such as `minikube start --cni=calico`.
- The demo disables host key checking (`StrictHostKeyChecking no`) because pods generate new host keys on every start.

## Layout

```
cmd/relay-client     ssh ProxyCommand
cmd/relay-server     relay (WebSocket -> pod sshd)
internal/auth        SSH-key challenge-response
internal/k8s         pod informer, annotation lookup
internal/wsconn      websocket net.Conn and pipe helpers
deploy/k8s.yaml      namespace, RBAC, relay, Service, Ingress, target pods, NetworkPolicy
Dockerfile.server    relay-server image
Dockerfile.target    sample target: Alpine + OpenSSH
```

## License

[MIT](LICENSE)
