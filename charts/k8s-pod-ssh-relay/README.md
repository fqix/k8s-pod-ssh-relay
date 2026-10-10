# k8s-pod-ssh-relay Helm chart

English | [简体中文](#简体中文)

Installs the [k8s-pod-ssh-relay](https://github.com/fqix/k8s-pod-ssh-relay) relay: the relay Deployment, its Service, an Ingress for `/connect`, `/ticket` and `/vscode/`, RBAC to watch pods, and the key that signs browser sessions. Target pods are not part of the chart. Any pod becomes reachable once annotated:

```bash
kubectl annotate pod <pod> k8s-pod-ssh-relay/ssh=true \
  k8s-pod-ssh-relay/authorized-keys="$(cat ~/.ssh/id_ed25519.pub)"
```

## Install

```bash
helm install k8s-pod-ssh-relay oci://ghcr.io/fqix/charts/k8s-pod-ssh-relay \
  --version <version> \
  -n k8s-pod-ssh-relay --create-namespace \
  --set ingress.host=relay.example.com \
  --set 'ingress.tls[0].secretName=relay-example-com-tls' \
  --set 'ingress.tls[0].hosts[0]=relay.example.com'
helm test k8s-pod-ssh-relay -n k8s-pod-ssh-relay
```

Without `ingress.tls`, the Ingress serves plain HTTP, which is only suitable for local testing.

## Values

| Value | Default | Description |
|---|---|---|
| `image.repository` / `image.tag` | `ghcr.io/fqix/k8s-pod-ssh-relay` / the chart's `appVersion` | Released relay-server image (amd64 and arm64) |
| `replicaCount` | `2` | Relay replicas. Each target pod's connections go to one replica (consistent hashing at the Ingress) |
| `ingress.host` | `relay.example.com` | Host clients use: `relay-client -relay wss://<host>` |
| `ingress.tls` | `[]` | TLS for the host, with your domain's certificate |
| `ingress.className` | `nginx` | The annotations assume ingress-nginx: 1-hour proxy timeouts and `upstream-hash-by` |
| `watchNamespaces` | `[]` | Empty: all namespaces, through a ClusterRole. Otherwise: a Role in each listed namespace |
| `sshPort` | `22` | sshd port for pods without a `k8s-pod-ssh-relay/ssh-port` annotation |
| `sessionKey.existingSecret` | `""` | Secret (key `key`) that signs browser sessions. If empty, the chart creates a random key and keeps it across upgrades |
| `podDisruptionBudget.enabled` | `true` | With more than one replica, keep `minAvailable` replicas during disruptions |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | Usual scheduling settings |

The relay runs as non-root with a read-only root filesystem.

---

## 简体中文

安装 [k8s-pod-ssh-relay](https://github.com/fqix/k8s-pod-ssh-relay) 的 relay：relay Deployment、Service、暴露 `/connect`、`/ticket`、`/vscode/` 的 Ingress、监听 Pod 的 RBAC，以及签名浏览器会话的密钥。target Pod 不属于 chart，任何 Pod 加上注解即可连接：

```bash
kubectl annotate pod <pod> k8s-pod-ssh-relay/ssh=true \
  k8s-pod-ssh-relay/authorized-keys="$(cat ~/.ssh/id_ed25519.pub)"
```

### 安装

```bash
helm install k8s-pod-ssh-relay oci://ghcr.io/fqix/charts/k8s-pod-ssh-relay \
  --version <version> \
  -n k8s-pod-ssh-relay --create-namespace \
  --set ingress.host=relay.example.com \
  --set 'ingress.tls[0].secretName=relay-example-com-tls' \
  --set 'ingress.tls[0].hosts[0]=relay.example.com'
helm test k8s-pod-ssh-relay -n k8s-pod-ssh-relay
```

不设置 `ingress.tls` 时，Ingress 使用明文 HTTP，只适合本地测试。

### 配置项

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `image.repository` / `image.tag` | `ghcr.io/fqix/k8s-pod-ssh-relay` / chart 的 `appVersion` | 发布的 relay-server 镜像（amd64 和 arm64） |
| `replicaCount` | `2` | relay 副本数。同一个 target Pod 的连接固定到一个副本（Ingress 一致性哈希） |
| `ingress.host` | `relay.example.com` | 客户端使用的域名：`relay-client -relay wss://<host>` |
| `ingress.tls` | `[]` | 该域名的 TLS，使用你的域名证书 |
| `ingress.className` | `nginx` | 注解按 ingress-nginx 编写：1 小时代理超时和 `upstream-hash-by` |
| `watchNamespaces` | `[]` | 为空：通过 ClusterRole 监听所有命名空间；否则在每个列出的命名空间创建 Role |
| `sshPort` | `22` | 没有 `k8s-pod-ssh-relay/ssh-port` 注解的 Pod 的 sshd 端口 |
| `sessionKey.existingSecret` | `""` | 签名浏览器会话的 Secret（键名 `key`）。为空时 chart 生成随机密钥，并在升级时保留 |
| `podDisruptionBudget.enabled` | `true` | 多副本时，在中断期间至少保留 `minAvailable` 个副本 |
| `resources`、`nodeSelector`、`tolerations`、`affinity`、`topologySpreadConstraints` | 空 | 常规调度配置 |

relay 以非 root 用户运行，根文件系统只读。
