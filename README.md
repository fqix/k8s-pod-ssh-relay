# k8s-pod-ssh-relay

ssh 通过 `ProxyCommand` 经 relay 连接到 minikube 中 Pod 的原生 sshd，Pod 不暴露任何 Service/端口。

```
ssh ──stdio──> relay-client ──wss──> Ingress (TLS, k8s-pod-ssh-relay.test) ──ws──> relay-server (ClusterIP) ──TCP──> <podIP>:22 (sshd)
```

- `cmd/relay-client`：ssh 的 ProxyCommand，用 `-i` 指定的 SSH 私钥向 relay 认证后，把 stdin/stdout 接到 WebSocket。私钥有密码时使用 ssh-agent 里对应的 key。
  - `-ca`：额外信任的 CA（demo 用 Makefile 生成的自签 CA）。
  - `-connect-to ip:port`：TCP 直连该地址，Host 头与 TLS 校验仍用 URL 里的域名（类似 curl `--connect-to`），因此 `k8s-pod-ssh-relay.test` 无需 DNS 或改 `/etc/hosts`。
- Ingress（minikube `ingress` addon / ingress-nginx）：唯一入口，TLS 在此终止，只暴露 `/connect`；读写超时调到 3600s，避免空闲 ssh 会话被默认 60s 超时断开。relay-server 的 Service 为 ClusterIP。
- `cmd/relay-server`：用 SSH 公钥认证客户端，从 Pod Informer 本地缓存把 Pod 名解析为 `PodIP:端口`，直连 sshd 并双向转发。
  - 只有 `k8s-pod-ssh-relay` 命名空间中带注解 `k8s-pod-ssh-relay/ssh: "true"` 的 Pod 可连；可选注解 `k8s-pod-ssh-relay/ssh-port` 覆盖默认端口 22。
  - 认证（`internal/auth`）：WebSocket 建立后，relay 下发 32 字节随机 nonce，客户端用私钥对 `(nonce, 目标 Pod 名)` 签名，relay 用**目标 Pod 的注解 `k8s-pod-ssh-relay/authorized-keys`**（authorized_keys 格式，可多行）校验。nonce 防重放，签名绑定目标 Pod；Pod 不存在与 key 未授权对客户端统一返回 `access denied`，详细原因只记在 relay 日志（含公钥指纹）。
  - 注解不能做服务端筛选，所以 Informer watch 整个命名空间的 Pod，在本地判断注解；RBAC 只需 `pods` 的 `list`/`watch`。
- `Dockerfile.target`：普通 alpine + OpenSSH sshd，没有任何 relay 相关组件。sshd 通过 downward API 卷读取同一个注解（`AuthorizedKeysCommand`，因为 downward API 挂载目录是 1777，`AuthorizedKeysFile` 会被 StrictModes 拒绝）。

### 按 Pod 授权

注解是 key 的唯一来源，relay（Informer，立即生效）和 sshd（downward API，kubelet 同步，最长约 1 分钟）都会跟随变化，无需重启：

```bash
# 给某个 Pod 增加一个 key
kubectl -n k8s-pod-ssh-relay annotate --overwrite pod <pod> \
  k8s-pod-ssh-relay/authorized-keys="$(cat keys/id_ed25519.pub ~/.ssh/other.pub)"
```

`make deploy` 会把 `keys/id_ed25519.pub` 填入 Deployment 模板中的注解。

## 使用

```bash
minikube addons enable ingress
make all      # 编译、在 minikube 内构建镜像、生成 SSH key 和 TLS 证书、部署、生成 ssh_config
make pods     # 查看可连接的 Pod
make ssh      # 登录第一个 target Pod
ssh -F ssh_config <pod-name>.relay
scp -F ssh_config file <pod-name>.relay:/tmp/
make clean
```

### 端口转发：直接用 ssh 的 -L / -R / -D

relay 只承载一条字节流，端口转发是 SSH 连接内部的 channel，原生支持（target 镜像已开启 `AllowTcpForwarding yes`）：

```bash
# 本机 2224 -> 经 Pod 访问集群内任意地址
ssh -F ssh_config -N -L 2224:kubernetes.default.svc:443 <pod-name>.relay
# Pod 内 127.0.0.1:9000 -> 本机 127.0.0.1:3000
ssh -F ssh_config -N -R 9000:127.0.0.1:3000 <pod-name>.relay
# SOCKS5 代理，经 Pod 出口
ssh -F ssh_config -N -D 1080 <pod-name>.relay
```

### 不使用 ssh_config 时

```bash
ssh -i keys/id_ed25519 \
  -o ProxyCommand="./bin/relay-client -relay wss://k8s-pod-ssh-relay.test -ca keys/ca.crt -connect-to $(minikube ip):443 -i keys/id_ed25519 -target %h" \
  demo@<pod-name>
```

如果 `k8s-pod-ssh-relay.test` 能解析到 Ingress（真实 DNS 或 `/etc/hosts`），可以省略 `-connect-to`；使用受信任证书时可以省略 `-ca`。
