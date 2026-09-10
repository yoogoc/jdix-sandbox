# 本地调试

## 先跑一次自检

```sh
make dev-preflight        # 或 hack/dev/preflight.sh
```

它检查工具链、集群是不是本地的、CRD 装没装、镜像在不在节点上，最后测一件决定性的事：**这台机器能不能路由到集群的 Pod 网段**。

之所以要测这个：沙箱 Pod 按设计不建 Service（§D2），controller 和 gateway 是直接连 `podIP:8081` / `podIP:8080` 的。宿主机能不能走通这条路，直接决定了调试布局。

---

## 选哪条路

| 你在调什么 | 用什么 | 需要 Pod 网络可达吗 |
|---|---|---|
| `pkg/bwrap` 参数生成、`pkg/safepath` 路径校验 | `go test ./pkg/bwrap ./pkg/safepath` + `make fuzz` | 不需要，也不需要集群 |
| `pkg/initd`（exec / PTY / 文件 API）、`pkg/execd` | `hack/dev/local-docker.sh` | 不需要，不用集群 |
| 隔离档位本身（bwrap 在真实内核上的行为） | `hack/dev/sandbox.sh` | 不需要（走 port-forward） |
| `pkg/controller`（绑定、预热池、准入） | `hack/dev/run.sh controller` | **需要** |
| `pkg/apiserver`（API Key、配额、幂等） | `hack/dev/run.sh apiserver` | 不需要 |
| `pkg/gateway`（路由、反代） | `hack/dev/run.sh gateway` | 不需要（默认走 API server 代理） |
| 两个 SDK | 指向本地 apiserver，见下 | 不需要 |

### Pod 网络不通怎么办

controller 有个开关专门解决这件事：

```sh
hack/dev/run.sh controller                      # 默认就是 apiserver-proxy
TRANSPORT=direct hack/dev/run.sh controller     # 网络通的话用这个，更快
```

`--execd-transport=apiserver-proxy` 让 controller 通过 Kubernetes API server 的 **pod proxy 子资源**去连 execd：

```
POST /api/v1/namespaces/{ns}/pods/http:{pod}:8081/proxy/internal/v1/bind
```

API server 本来就能连到每个 Pod，而你已经有 kubeconfig 了。所以整个 controller 可以在集群外调试，Pod 网络通不通都无所谓。

**注意 port-forward 在这里救不了场**：bind 的目标是预热池临时交出来的那个 Pod，事先不知道是哪个，没法提前 forward。

**这不是生产用的 transport。**每次 bind、unbind、probe 都变成一次 API server 的代理请求——把系统里最忙的路径压到最不该成为瓶颈的组件上。生产用 `direct`，它是默认值。需要 RBAC `pods/proxy`（已在 `config/rbac/role.yaml` 里）。

gateway 有同样的逃生口，flag 叫 `--sandbox-transport=direct|apiserver-proxy`，`hack/dev/run.sh` 里默认也是 `apiserver-proxy`。`kubectl port-forward` 同样救不了场：gateway 代理到的是请求进来时那个沙箱绑定的 Pod，事先不知道是哪个。

**gateway 走这条路要多做两件事**，都在 `pkg/gateway/transport.go` 里：

1. **token 必须从 `Authorization` 挪走。** API server 会剥掉这个头（这是对的：调用方的集群凭据不该落到 workload 里），而且 client-go 的 transport 看到已有的 `Authorization` 就不会覆盖——沙箱 token 会被当成 gateway 自己的凭据送给 API server。两条路都通向 401。所以走这条 transport 时 token 改用 `X-Jdix-Control-Token`，execd 本来就认这个头。
2. **寻址方式变了。** 直连用 `podIP:port`，pod proxy 用 `namespace/podName`。没绑定到具名 Pod 的沙箱会得到一个说明清楚的 502，而不是更下游某处的怪错误。

WebSocket 能穿过去，这一点实测过：client-go 的 transport 和 API server 协商的是 HTTP/2，而 HTTP/2 明令禁止 `Connection` 与 `Upgrade` 头——但 net/http 遇到带 `Upgrade` 的请求会退回 HTTP/1.1，所以同一个 transport 上普通请求走 h2、升级请求走 HTTP/1.1 并拿到真正的 101。不需要手动钉协议。

**这条路比 controller 那条更不能用于生产**：controller 是每次 bind 一个代理请求，gateway 是 exec、PTY、文件传输的**每一个字节**都变成 API server 流量。

### 另一条路：envtest，完全不要集群

如果你要调的是 controller 的**逻辑**（CAS 抢占、池供给、TTL、finalizer、准入），而不是它和 execd 的真实交互，`envtest` 更合适——它在本地起一个真的 kube-apiserver + etcd，没有 kubelet，所以 Pod 永远不会真的运行，你自己写它的 status：

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS=$(setup-envtest use -p path)
```

好处是拿到了真实 API server 的全部语义（CRD 校验、finalizer、乐观并发、watch），代价是 Binder 只能用桩。`pkg/controller` 现有的 fake-client 测试已经覆盖了大部分这类逻辑，envtest 适合追那些 fake client 模拟不出来的问题（比如 resourceVersion 冲突的真实行为）。

---

## 典型流程

```sh
make install-crds                 # 一次就够
make image load-image             # 每次改 execd / jdix-init 后重来

hack/dev/up.sh                    # 建 namespace、token、模板、预热池
hack/dev/run.sh controller        # 终端 1
hack/dev/run.sh apiserver         # 终端 2，会打印一个可用的 API key
hack/dev/trace.sh                 # 终端 3，看池和沙箱状态

hack/dev/down.sh                  # 收工
```

不需要 controller 的时候（大多数数据面工作）：

```sh
hack/dev/sandbox.sh               # 起一个 Pod、bind、给你一个 shell
hack/dev/sandbox.sh --args        # 只看生成的 bwrap argv，不进 shell
hack/dev/sandbox.sh -- 'ls -la /workspace'
```

完全不用集群：

```sh
hack/dev/local-docker.sh          # docker run + bind + shell
PRIVILEGED=1 hack/dev/local-docker.sh   # 想要 userns 档位就得这样
```

## 断点

```sh
DLV=1 hack/dev/run.sh controller       # 在 :2345 等你接上
dlv connect :2345                      # 或者 IDE 里连 remote
```

`dlv debug` 会重新编译，所以断点打在源码上就行。多客户端已开（`--accept-multiclient`），IDE 断开不会杀掉进程。

---

## 已知的摩擦点

这些是实际会绊住人的地方，不是理论问题。

**1. 沙箱 endpoint 硬编码 https。**
`jdix-controller` 的 `--endpoint-suffix` 拼出来的是 `https://{id}.{suffix}`（`cmd/jdix-controller/main.go` 里写死的 scheme）。本地没有 TLS，也没有那套 DNS，所以 SDK 拿到的 endpoint 直连不上。

绕法：用 `hack/dev/sandbox.sh`，它走 port-forward，压根不碰 endpoint。要让 SDK 端到端跑通，最小改动是给 controller 加一个 `--endpoint-scheme` flag（一行），本地设成 `http`。

**2. apiserver `--dev-seed` 的租户命名空间是 `tenant-dev`。**
它写死在 `cmd/jdix-server/main.go` 的 `seed()` 里，而 `hack/dev/up.sh` 默认建的是 `jdix-dev`。通过 apiserver 创建沙箱时两边要对齐：

```sh
NS=tenant-dev hack/dev/up.sh
NS=tenant-dev hack/dev/run.sh controller
```

**3. 本地构建的镜像解析不了——用 `resolve: false`。**
模板正常写 tag 就行，controller 会解析一次并钉住 digest。但本地 `docker build` 出来的镜像从没 push 过，任何 registry 都看不到它。

模板里显式关掉就行：

```yaml
spec:
  image:
    ref: jdix/sandbox-base:dev
    resolve: false        # 不问 registry
    pullPolicy: Never     # 镜像已经在节点上（hack/load-image.sh 放的）
```

`hack/dev/up.sh` 就是这么写的。代价是 `status.imagePinned` 变成 false——同一个池里的 Pod 理论上可能跑不同构建。本地调试无所谓，生产上要清楚自己在换什么。

**4. 没有 controller 在跑时，namespace 删不掉。**
Sandbox 带 finalizer（这正是"删了 Sandbox 一定会删掉 Pod"的保证）。没人清 finalizer，`kubectl delete ns` 会一直卡住。`hack/dev/down.sh` 会检测并告诉你怎么强拆。

**5. 走 apiserver-proxy 时 `Authorization` 头会被剥掉。**
Kubernetes API server 在把请求转发给 Pod 时会移除 `Authorization`——这是有意的，避免调用者的集群凭据泄漏进工作负载。所以控制面 token 走的是 `X-Jdix-Control-Token`，execd 两个头都接受。

症状是 `probe` 正常（它不需要鉴权）但 `bind` 返回 `401 invalid control-plane token`。这两件事一起出现基本就能确诊：

```sh
go run ./hack/dev/proxyprobe <namespace> <pod>
```

它用 controller 同一套客户端跑一次 probe 和一次 bind，把三种 401 分开：Pod 连不上、Pod 没有凭据、凭据没送到。

**6. gateway 走 proxy 时 token 换了个头。**
`--sandbox-transport=apiserver-proxy` 下，沙箱 token 走 `X-Jdix-Control-Token` 而不是 `Authorization`——API server 会剥掉后者。如果你手搓 curl 去复现一个 401，注意这个差别：直连时两个头都行，走 proxy 时只有前者能到。用户端口（`/p/{port}/`）则两个都不给，租户自己的应用和我们没有这个约定。

**7. 纯 docker 模式拿不到 userns 档位。**
普通容器没办法授予非特权 user namespace，`local-docker.sh` 会测出 `chroot`，此时 `spec.filesystem.mounts` 会被拒绝。要调隔离本身就得用 `sandbox.sh`，或者加 `PRIVILEGED=1`（`--privileged` 顺带解开了 /proc 遮蔽和 seccomp，也就是 k8s 里那三项的等价物）。

---

## 症状对照

隔离档位掉到 `chroot` 时，`/internal/v1/probe` 的 `probes` 字段里有原始报错。三种常见的：

| 报错 | 原因 | 解法 |
|---|---|---|
| `No permissions to create new namespace` | `seccompProfile: RuntimeDefault` 禁止没有 `CAP_SYS_ADMIN` 的容器调用带 `CLONE_NEWUSER` 的 `unshare` | Pod 上设 `seccompProfile: Unconfined`（并接受 §3.5 的取舍），或做一个只放开这一项的自定义 profile |
| `Can't mount proc on /newroot/proc` | k8s 用 bind mount 遮蔽了 `/proc` 一部分，bwrap 挂新 procfs 时内核 `mount_too_revealing()` 拒绝 | 容器上设 `procMount: Unmasked` |
| API server 拒绝 `procMount: Unmasked` | 这个字段要求 Pod 在自己的 user namespace 里 | Pod 上设 `hostUsers: false` |

三项都对的最小清单：`config/samples/tier-a-pod.yaml`。

看某个具体沙箱：

```sh
hack/dev/trace.sh                # 总览
hack/dev/trace.sh sbx-abc123     # 单个沙箱的 CR + Pod + execd 日志
hack/dev/trace.sh -f sbx-abc123  # 跟日志
```

---

## SDK

```sh
hack/dev/run.sh apiserver        # 打印 dev API key

export JDIX_API_KEY='jdix_sk_...'
export JDIX_BASE_URL=http://127.0.0.1:8000

cd sdk/python && .venv/bin/python -m pytest       # 或者写个脚本调真实服务
cd sdk/go && go test ./...
```

两个 SDK 的测试都自带假服务端，不需要集群。要打真实服务端时，注意上面第 1 条：拿到的 endpoint 是 https，数据面调用会失败，先用 port-forward 或加那个 scheme flag。
