# Remote - PodRelay

English | [简体中文](#简体中文)

Open a VS Code remote window inside a Kubernetes pod through [PodRelay](https://github.com/fqix/podrelay), similar to Remote - SSH.

- **No external binaries.** The relay protocol (WebSocket plus the SSH-key challenge-response) and SSH (via `ssh2`) are implemented in TypeScript inside the extension. `podrelay` and `ssh` are not used.
- **Tunnel-style resolver.** Like the Remote - Tunnels extension, it resolves to a `ManagedResolvedAuthority`: every connection VS Code needs is an SSH `direct-tcpip` channel to the VS Code server in the pod, and no local port is opened.
- **Works without internet in the pod.** The VS Code server matching your editor's commit is downloaded on your machine (and cached), then uploaded to the pod over SSH. glibc and musl (Alpine) pods on x64/arm64 are supported.
- **Reconnects.** If the relay or network drops, the SSH session is rebuilt on the next connection VS Code makes, and the remote extension host keeps its state.

## Requirements

- An editor that allows the `resolvers` proposed API for this extension:
  - VS Code: start it with `--enable-proposed-api fqix.remote-podrelay`, or add the id to `"enable-proposed-api"` in `argv.json` (run "Preferences: Configure Runtime Arguments").
  - VSCodium and Code - OSS work the same way.
- In the pod: `sshd` with `AllowTcpForwarding yes`, `tar`, and for musl pods `libstdc++` and `libgcc`.

## Settings

| Setting | Description |
|---|---|
| `podrelay.relayUrl` | Relay URL: `wss://relay.example.com`, or `ws://` for local testing |
| `podrelay.identityFile` | Private key (default `~/.ssh/id_ed25519`). If it is passphrase-protected, the matching key in ssh-agent is used |
| `podrelay.user` | Login user in the pod (default `root`) |
| `podrelay.connectTo` | Dial this `host:port` instead of resolving the relay host, like curl `--connect-to` |
| `podrelay.serverDownloadUrlTemplate` | Where to download the VS Code server from. Defaults to the editor's own `product.json` template, or Microsoft's update server |

## Usage

Run **Remote-PodRelay: Connect to Pod...** from the Command Palette or the remote indicator menu and enter `namespace/pod`, or open a URI directly. A remote authority can't contain `/`, so the URI writes the target as `<pod>.<namespace>`:

```bash
code --enable-proposed-api fqix.remote-podrelay \
  --folder-uri "vscode-remote://podrelay+<pod>.<namespace>/home/demo"
```

Logs: **Remote-PodRelay: Show Log**.

## Build

```bash
npm install
npm run package          # typecheck, bundle with esbuild, produce the .vsix
code --install-extension remote-podrelay-*.vsix
```

`npm run build:e2e && node dist/e2e.js <namespace>/<pod>` runs the whole chain without VS Code: relay, SSH, server install, and a `GET /version` against the server. It is configured through the `RELAY_URL`, `RELAY_CONNECT_TO` and `IDENTITY_FILE` environment variables.

---

## 简体中文

通过 [PodRelay](https://github.com/fqix/podrelay) 在 Kubernetes Pod 中打开 VS Code 远程窗口，类似 Remote - SSH。

- **不依赖外部程序**：relay 协议（WebSocket + SSH 公钥 challenge-response）和 SSH（`ssh2`）都在扩展内用 TypeScript 实现，不使用 `podrelay` 和 `ssh`。
- **Tunnel 式 resolver**：和 Remote - Tunnels 扩展一样，解析为 `ManagedResolvedAuthority`。VS Code 需要的每条连接都是一条到 Pod 内 VS Code Server 的 SSH `direct-tcpip` channel，本地不监听任何端口。
- **Pod 无需外网**：与编辑器 commit 一致的 VS Code Server 在本机下载并缓存，再通过 SSH 上传到 Pod。支持 x64/arm64 的 glibc 和 musl（Alpine）Pod。
- **自动重连**：relay 或网络断开后，VS Code 下一次建立连接时会重建 SSH 会话，远程扩展宿主保持原有状态。

### 要求

- 编辑器需要允许本扩展使用 `resolvers` proposed API：
  - VS Code：启动时加 `--enable-proposed-api fqix.remote-podrelay`，或者把扩展 id 加入 `argv.json` 的 `"enable-proposed-api"`（命令 "Preferences: Configure Runtime Arguments"）。
  - VSCodium 和 Code - OSS 用同样的方式。
- Pod 内需要：开启 `AllowTcpForwarding yes` 的 `sshd`、`tar`；musl Pod 还需要 `libstdc++` 和 `libgcc`。

### 设置

| 设置 | 说明 |
|---|---|
| `podrelay.relayUrl` | relay 地址：`wss://relay.example.com`，本地测试用 `ws://` |
| `podrelay.identityFile` | 私钥（默认 `~/.ssh/id_ed25519`）。私钥有密码时使用 ssh-agent 中对应的 key |
| `podrelay.user` | Pod 内登录用户（默认 `root`） |
| `podrelay.connectTo` | TCP 直连这个 `host:port`，不解析 relay 主机名，类似 curl `--connect-to` |
| `podrelay.serverDownloadUrlTemplate` | VS Code Server 下载地址。默认使用编辑器自身 `product.json` 中的模板，否则使用微软更新服务器 |

### 使用

在命令面板或左下角远程菜单运行 **Remote-PodRelay: Connect to Pod...** 并输入 `namespace/pod`，或直接打开 URI。远程 authority 不能包含 `/`，所以 URI 中写作 `<pod>.<namespace>`：

```bash
code --enable-proposed-api fqix.remote-podrelay \
  --folder-uri "vscode-remote://podrelay+<pod>.<namespace>/home/demo"
```

日志：**Remote-PodRelay: Show Log**。

### 构建

```bash
npm install
npm run package          # 类型检查、esbuild 打包、生成 .vsix
code --install-extension remote-podrelay-*.vsix
```

`npm run build:e2e && node dist/e2e.js <namespace>/<pod>` 可以不启动 VS Code 跑通整条链路：relay、SSH、安装 Server，并对 Server 发送 `GET /version`。通过环境变量 `RELAY_URL`、`RELAY_CONNECT_TO`、`IDENTITY_FILE` 配置。
