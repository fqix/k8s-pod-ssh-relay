# k8s-pod-ssh-relay

[![CI](https://github.com/fqix/k8s-pod-ssh-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/fqix/k8s-pod-ssh-relay/actions/workflows/ci.yml)

[English](README.md) | 简体中文

通过 WebSocket relay 作为 ssh 的 `ProxyCommand`，登录运行原生 `sshd` 的 Kubernetes Pod。目标 Pod 不对集群外暴露任何 Service 或端口，也不需要 agent 或 sidecar。

```mermaid
flowchart TB
  subgraph local["本机"]
    direction LR
    ssh["ssh / scp"] <-->|stdio| client["relay-client"]
  end
  subgraph cluster["Kubernetes 集群"]
    direction LR
    ingress["Ingress"] <-->|ws| relay["relay-server"] <-->|TCP| sshd["sshd<br/>podIP:22"]
  end
  local <-->|wss| cluster
```

## 特性

- **原生 ssh**：支持 `ssh`、`scp`、`sftp`、`rsync -e ssh`、VS Code Remote-SSH，以及端口转发（`-L` / `-R` / `-D`）。
- **按 `namespace/pod` 连接**：`ssh <pod>.<namespace>.relay`。relay 监听所有命名空间（或只监听 chart 中 `watchNamespaces` 列出的命名空间）的 Pod，从 Informer 本地缓存解析，每次连接都不需要请求 API Server。
- **注解开启**：只有带注解 `k8s-pod-ssh-relay/ssh: "true"` 的 Pod 才可以连接。
- **SSH 公钥认证，按 Pod 授权**：客户端必须证明自己持有目标 Pod 注解 `k8s-pod-ssh-relay/authorized-keys` 中列出的某个私钥。sshd 读取同一个注解，因此这个注解是 key 的唯一来源。修改后无需重启即可生效。
- **单一 Ingress 入口**：只暴露 `/connect`、`/ticket` 和 `/vscode/`。生产环境在此终止域名的 TLS（`wss://`、`https://`）；minikube demo 使用明文 HTTP。
- **VS Code 扩展 Remote - PodRelay**：从左下角远程菜单直接在 Pod 中打开远程窗口，不需要额外的程序。详见 [VS Code](#vs-coderemote---podrelay)。

## 架构

```mermaid
flowchart TB
  subgraph local["本机"]
    direction LR
    ssh["ssh / scp / VS Code"] <-->|stdio| client["relay-client<br/>(ProxyCommand)"]
  end

  subgraph cluster["Kubernetes 集群"]
    direction LR
    ingress["Ingress (ingress-nginx)<br/>/connect、/ticket、/vscode/"]
    api[("API Server")]
    subgraph ns["命名空间 k8s-pod-ssh-relay"]
      relay["relay-server<br/>Service: ClusterIP"]
    end
    subgraph nsA["命名空间 team-a"]
      podA["Pod A<br/>sshd :22"]
    end
    subgraph nsB["命名空间 team-b"]
      podB["Pod B<br/>sshd :22"]
    end
    ingress <-->|ws| relay
    relay <-->|"TCP podIP:22"| podA
    relay -. "TCP podIP:22" .-> podB
    relay -. "list/watch pods（Informer）" .-> api
  end

  local <-->|"wss://host/connect?target=ns/pod"| cluster
```

`ssh` 与 `sshd` 之间端到端进行 SSH。relay 只负责认证客户端、选择 Pod、转发字节。

### 一次连接的过程

```mermaid
sequenceDiagram
  autonumber
  participant S as ssh
  participant C as relay-client
  participant R as relay-server
  participant I as Informer 缓存
  participant P as Pod sshd

  S->>C: 启动 ProxyCommand（-target ns/pod）
  C->>R: WebSocket /connect?target=ns/pod（经 Ingress）
  R->>C: challenge：随机 nonce
  C->>R: 公钥 + 对 (nonce, pod) 的签名
  R->>I: 读取 Pod 注解
  Note over R: 公钥在 authorized-keys 中？<br/>签名有效？<br/>否则返回 "access denied"
  R->>P: TCP 连接 podIP:ssh-port
  R->>C: ok
  S-->>P: SSH 握手、认证、会话（relay 只转发字节）
```

## 工作原理

1. `ssh` 把 `relay-client` 作为 `ProxyCommand` 启动，通过它的 stdin/stdout 收发 SSH 数据。
2. `relay-client` 经 Ingress 连接 `wss://<host>/connect?target=<namespace>/<pod>`。不能使用 `/` 的地方（ssh 主机名、VS Code 远程 authority）写作 `<pod>.<namespace>`。命名空间不能包含点，所以用最后一个点分隔。
3. **认证。** relay 下发 32 字节随机 nonce，客户端用私钥对 `(nonce, 目标 Pod 名)` 签名，relay 用目标 Pod 的 `authorized-keys` 注解校验签名。
   - nonce 防止重放；签名中包含 Pod 名，所以签名只对这个 Pod 有效。
   - Pod 不存在和 key 未授权，对客户端统一返回 `access denied`，防止探测哪些 Pod 存在。relay 日志会记录真实原因和公钥指纹。
4. relay 从 Informer 缓存查到 Pod IP，直连 Pod 的 sshd，之后双向转发字节。
5. SSH 在本地 `ssh` 与 Pod 内 `sshd` 之间端到端进行，所以 relay 只能看到加密后的 SSH 流量。

### Pod 注解

| 注解 | 必需 | 含义 |
|---|---|---|
| `k8s-pod-ssh-relay/ssh` | 是 | 必须为 `"true"`，Pod 才可以连接 |
| `k8s-pod-ssh-relay/authorized-keys` | 是 | 允许的公钥，`authorized_keys` 格式，每行一个 |
| `k8s-pod-ssh-relay/ssh-port` | 否 | sshd 端口（默认 `22`） |

示例 target 镜像就是普通的 Alpine + OpenSSH。sshd 通过 `AuthorizedKeysCommand` 从 downward API 卷读取 `authorized-keys`。不能用 `AuthorizedKeysFile`，因为 downward API 挂载目录的权限是 `1777`，sshd 的 `StrictModes` 检查会拒绝它。

## 快速开始（minikube）

依赖：minikube、kubectl、Helm、Go。

```bash
minikube addons enable ingress
make all
```

`make all` 会依次：
- 编译二进制
- 在 minikube 内构建镜像
- 在 `keys/` 下生成 SSH key
- 用 Helm chart 安装 relay，并部署示例 target Pod，均在 `k8s-pod-ssh-relay` 命名空间
- 生成 `ssh_config`

```bash
make pods                                # 查看 Pod 及其注解
make ssh                                 # 登录第一个 target Pod
ssh -F ssh_config <pod>.<namespace>.relay
scp -F ssh_config file <pod>.<namespace>.relay:/tmp/
```

生成的 `ssh_config` 匹配 `Host *.relay`，执行：

```bash
relay-client \
  -relay ws://k8s-pod-ssh-relay.test \
  -connect-to <minikube-ip>:80 \
  -i keys/id_ed25519 \
  -target %h
```

如果不想每次加 `-F`，在 `~/.ssh/config` 开头加一行 `Include /path/to/ssh_config`。

### 在运行中的 Pod 上授权或撤销

```bash
kubectl -n k8s-pod-ssh-relay annotate --overwrite pod <pod> \
  k8s-pod-ssh-relay/authorized-keys="$(cat keys/id_ed25519.pub ~/.ssh/other.pub)"
```

- relay 通过 Informer 立即生效。
- sshd 要等 kubelet 同步 downward API 文件后才生效，最长约 1 分钟。

### 端口转发

端口转发是 SSH 连接内部的 channel，不需要 relay 做任何事：

```bash
# 本机 2224 -> Pod 能访问的任意地址
ssh -F ssh_config -N \
  -L 2224:kubernetes.default.svc:443 \
  <pod>.<namespace>.relay

# Pod 内 127.0.0.1:9000 -> 本机 3000
ssh -F ssh_config -N \
  -R 9000:127.0.0.1:3000 \
  <pod>.<namespace>.relay

# 经 Pod 出口的 SOCKS5 代理
ssh -F ssh_config -N \
  -D 1080 \
  <pod>.<namespace>.relay
```

### 浏览器中的 VS Code

`relay-client web` 在 Pod 中打开 VS Code for the Web，本地无需安装任何东西：

```bash
./bin/relay-client web \
  -relay ws://k8s-pod-ssh-relay.test \
  -connect-to $(minikube ip):80 \
  -i keys/id_ed25519 \
  -user demo \
  <namespace>/<pod>
```

1. 经 relay 通过 SSH 在 Pod 中安装 VS Code Server 的 Web 版，并在 13337 端口（`-pod-port`）以 `/vscode/<namespace>/<pod>/` 为路径前缀启动。Server 在本机下载（有缓存，中断可续传）后上传，所以 Pod 不需要外网。
2. 用同一个 SSH 密钥向 relay 认证，获得有效期 2 分钟的票据，然后在浏览器中打开 `<relay>/vscode/<namespace>/<pod>/?ticket=…`（生产环境为 `https://`，minikube demo 为 `http://`），命令随即退出。
3. relay 用票据换成 12 小时的会话 Cookie（作用域限定在该 Pod 的路径），并把所有请求（包括 WebSocket）代理到 Pod。只有 Pod `authorized-keys` 注解中的密钥才能拿到票据，VS Code 自身的连接 token 仍然叠加生效。

浏览器需要能解析 relay 域名。minikube demo 中，在 `/etc/hosts` 加入 `<minikube-ip> k8s-pod-ssh-relay.test`。也可以加 `-local`，改为通过 SSH 转发到本地端口访问，不经过 Ingress。

## relay 扩缩容

relay 不保存共享状态：认证在连接内部完成，每个副本运行自己的 Informer。因此可以水平扩展（默认 `replicas: 2`）。

Ingress 按目标 Pod 名做哈希（`nginx.ingress.kubernetes.io/upstream-hash-by: "$arg_target"`），所以同一个 Pod 的所有连接都经过同一个 relay 副本。哈希是一致性哈希：增减副本时，只有变动副本上的 Pod 会重新分配，约 1/N。

某个 relay 副本停止时（滚动更新、缩容、节点驱逐），它上面的会话会断开，客户端需要重连。

## VS Code：Remote - PodRelay

[`vscode/`](vscode/) 是 VS Code 扩展 **Remote - PodRelay**，可以在 Pod 中打开远程窗口，类似 Remote - SSH。你可以在 Pod 里打开文件夹、使用终端、调试和运行扩展。

- **不依赖外部程序**：不需要 `relay-client` 或 `ssh`。relay 协议和 SSH 都在扩展内用 TypeScript 实现。
- **Tunnel 式连接**：和 Remote - Tunnels 一样，解析为 `ManagedResolvedAuthority`。VS Code 的每条连接都是一条到 Pod 内 VS Code Server 的 SSH `direct-tcpip` channel，本地不监听任何端口。
- **Pod 无需外网**：与编辑器 commit 一致的 VS Code Server 在本机下载并缓存，再通过 SSH 上传。支持 x64 和 arm64 的 glibc、musl Pod。
- **自动重连**：relay 或网络断开后自动重连，远程扩展宿主保持原有状态。

### 安装

```bash
# 构建 vscode/remote-podrelay-0.1.0.vsix
make vscode
code --install-extension vscode/remote-podrelay-0.1.0.vsix

# 输出 demo 集群对应的设置
make vscode-settings
```

1. 扩展使用 `resolvers` proposed API，需要 VS Code 允许。运行 **Preferences: Configure Runtime Arguments**，在 `argv.json` 中加入 `"enable-proposed-api": ["fqix.remote-podrelay"]`，然后重启 VS Code。同样因为这个原因，扩展不能发布到 Visual Studio Marketplace。VSCodium 和 Code - OSS 也可以使用。
2. 把 `make vscode-settings` 的输出粘贴到用户 `settings.json` 中。

### 连接

点击左下角的远程按钮，选择 **Connect to Pod...**（分类 Remote-PodRelay），输入 `namespace/pod`。窗口会标记为 `Pod: namespace/pod`。也可以直接打开文件夹 URI；远程 authority 不能包含 `/`，所以写作 `<pod>.<namespace>`：

```bash
code --folder-uri \
  "vscode-remote://k8s-pod-ssh-relay+<pod>.<namespace>/home/demo"
```

日志见 **Remote-PodRelay: Show Log**。

Pod 需要开启 `AllowTcpForwarding yes` 的 `sshd` 和 `tar`。musl Pod 还需要 `libstdc++` 和 `libgcc`，示例 target 镜像已包含。完整设置以及构建和测试说明见 [vscode/README.md](vscode/README.md)。

## relay-client 参数

| 参数 | 说明 |
|---|---|
| `-relay` | relay 地址：生产环境 `wss://relay.example.com`，本地测试用 `ws://`（默认读取 `$RELAY_URL`） |
| `-target` | `namespace/pod`，或 ssh 主机名形式 `<pod>.<namespace>[.relay]`，所以可以直接传 `%h` |
| `-i` | 私钥（默认 `~/.ssh/id_ed25519`）。私钥有密码时使用 `ssh-agent` 中对应的 key，因为 ssh 占用了 stdin，客户端无法提示输入密码 |
| `-connect-to` | TCP 直连这个 `host:port`，不解析 URL 中的主机名。Host 头和 TLS 校验仍使用 URL 中的主机名，类似 curl 的 `--connect-to` |

如果 relay 域名有真实 DNS 记录，可以省略 `-connect-to`。`wss://` 的证书按系统信任库校验。

## 生产环境

minikube demo 使用明文 HTTP。生产环境安装已发布的 chart，它会运行 `ghcr.io/fqix/k8s-pod-ssh-relay` 中对应版本的镜像。用该域名的证书终止 TLS：通过 `ingress.tls`（例如 cert-manager 签发的 Secret），或者在负载均衡器上：

```bash
helm install k8s-pod-ssh-relay oci://ghcr.io/fqix/charts/k8s-pod-ssh-relay \
  --version <version> \
  -n k8s-pod-ssh-relay --create-namespace \
  --set ingress.host=relay.example.com \
  --set 'ingress.tls[0].secretName=relay-example-com-tls' \
  --set 'ingress.tls[0].hosts[0]=relay.example.com'
```

全部配置项见 [charts/k8s-pod-ssh-relay](charts/k8s-pod-ssh-relay/README.md)，例如 `watchNamespaces`、`replicaCount`、`sessionKey.existingSecret`。

- 客户端使用 `wss://relay.example.com`，不需要 `-connect-to`，证书按系统信任库校验。浏览器打开 `https://relay.example.com/vscode/…`，HTTPS 下会话 Cookie 自动带 `Secure`。
- 每个 [release](https://github.com/fqix/k8s-pod-ssh-relay/releases) 都附带 `relay-client` 二进制和 VS Code 扩展。
- `make ssh-config RELAY_URL=wss://relay.example.com INGRESS=` 和 `make vscode-settings RELAY_URL=wss://relay.example.com INGRESS=` 生成对应的客户端配置。

## 发布

推送 `v0.2.0` 这样的 tag 会运行发布流程，发布：

- `relay-client` 二进制（Linux、macOS 的 amd64 和 arm64，以及 Windows）、`relay-server` 二进制（Linux），附校验和
- amd64 和 arm64 的 `ghcr.io/fqix/k8s-pod-ssh-relay` 镜像（`:<version>`，正式版同时更新 `:latest`）
- Helm chart：`oci://ghcr.io/fqix/charts/k8s-pod-ssh-relay`
- VS Code 扩展（`remote-podrelay-<version>.vsix`）

发布说明根据上一个 tag 以来的 Conventional Commits 生成。带后缀的 tag（如 `v0.2.0-rc.1`）发布为预发布版本。

```bash
git tag v0.2.0
git push origin v0.2.0
```

## 安全说明

- 能修改被监听命名空间中 Pod 注解的人，就能给自己授予 SSH 权限。请用 RBAC 限制对 pods 的 `patch`，或者为 `k8s-pod-ssh-relay/*` 注解加 admission 策略。
- relay 只需要 pods 的 `list`/`watch` 权限。chart 默认通过 ClusterRole 授予所有命名空间的权限；设置 `watchNamespaces` 则改为在每个列出的命名空间中创建 Role。
- 自带的 NetworkPolicy 只允许 relay 访问 target Pod 的 22 端口。只有支持 NetworkPolicy 的 CNI 才会执行它，例如 `minikube start --cni=calico`。
- demo 关闭了主机密钥校验（`StrictHostKeyChecking no`），因为 Pod 每次启动都会生成新的主机密钥。

## 目录结构

```
cmd/relay-client     ssh ProxyCommand
cmd/relay-server     relay（WebSocket -> Pod sshd）
internal/relayproto  线路协议：SSH 公钥 challenge-response，客户端与服务端（包文档即协议规范）
internal/target      namespace/pod 目标名称及 <pod>.<namespace> 形式
internal/relayserver relay 的 HTTP 处理（/connect、/healthz）
internal/podresolver Pod Informer、注解查询
internal/relayclient 连接 relay 并认证
internal/podssh      到 Pod 的 SSH 客户端（自动重连）
internal/vscodeserver 在 Pod 中安装、启动 VS Code for the Web（scripts/ 通过 go:embed 内嵌）
internal/wsconn      WebSocket 转 net.Conn
internal/netutil     双向 Pipe
charts/k8s-pod-ssh-relay  relay 的 Helm chart：RBAC、Deployment、Service、Ingress、会话密钥、PDB
deploy/demo-target.yaml   示例 target Pod 及其 NetworkPolicy
Dockerfile.server    relay-server 镜像
Dockerfile.target    示例 target：Alpine + OpenSSH
vscode/              VS Code 扩展（远程 resolver，TypeScript 实现的 relay 协议）
```

## 许可证

[MIT](LICENSE)
