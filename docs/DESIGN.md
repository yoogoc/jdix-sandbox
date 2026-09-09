# jdix-sandbox 架构设计

> 版本 v0.3 · 2026-09-08
> 目标：为 AI Agent 提供安全、秒开、可计量的代码执行沙箱。参考 OpenSandbox 与 kubernetes-sigs/agent-sandbox。

**v0.3 相对 v0.2 的变更**

| # | 变更 | 连锁影响 |
|---|---|---|
| 1 | **预热成本由平台统一承担** | 预热池从"租户自助"变为"平台配额分配"，见 §05.6、§06.1 |
| 2 | **不使用 gVisor**，且不做自定义 seccomp profile | 隔离天花板需明说并接受，见 §03.5 |
| 3 | **volume 支持任意 CSI / NFS** | 两阶段优化变得可行；但引入 RWX 约束与 attach 延迟，见 §05.4 |
| 4 | **全局禁用 gRPC，一律 HTTP / WS** | execd 控制面、controller→execd、execd→init 全部改协议，见 §07 |

**v0.2 相对 v0.1 的变更（保留备查）**：项目更名 `jdi-` → `jdix-`；引入 `Sandbox` CRD；允许租户自定义镜像；目标并发 ≤1000；管理平台 OIDC；多租户同节点混跑；bwrap 改为 Bind 后按 spec 施加。

---

## 0. 前提

### 0.1 已确定的选择

| 决策 | 取值 |
|---|---|
| Pod 模型 | 一次性：一个 Sandbox 独占一个 Pod，销毁即删 Pod |
| 编排事实源 | `Sandbox` CRD |
| 沙箱能力 | Shell 执行（同步/流式/PTY）、文件读写、端口暴露 |
| 状态 | 临时 + TTL，无 PVC 持久化、无 pause/resume |
| 内核前提 | 未知，三档降级 + 探测（§03） |
| 镜像 | 平台模板 + 租户自定义镜像，需准入 |
| 目标并发 | ≤ 1000 沙箱 |
| 管理平台登录 | OIDC |
| 租户边界 | 同节点混跑 |
| **预热成本** | **平台统一承担 → 预热池由平台按预算分配** |
| **沙箱运行时** | **不用 gVisor / Kata；隔离天花板 = 共享内核 + user namespace** |
| **数据卷** | **任意 CSI / NFS，不限 StorageClass；受 RWX 约束；租户长期持有** |
| **通信协议** | **全链路 HTTP / WebSocket，不使用 gRPC** |

### 0.2 目标

1. 命中预热池时 `create → ready` P99 < 300ms；无卷模板冷启动 < 5s（镜像已预拉）。
2. 沙箱内即使拿到 root，也读不到 SA token、execd 二进制与凭据；只能看到 Sandbox spec 声明的目录。
3. 控制面无状态可水平扩展。
4. 租户 → API Key → 配额 → 用量 全链路可管可算。
5. Python / Go SDK 达到"能替代本地 subprocess"的易用度。

### 0.3 并发 ≤ 1000 带来的简化

- **不需要 Redis**。绑定用 Kubernetes 乐观锁，配额计数走 Postgres 行锁 + 内存缓存足够。
- **不需要分库分表**。稳态 ≈ 0.6 创建/秒，峰值按 20/秒设计，单 Postgres 实例绰绰有余。
- **单 controller 副本 + leader election** 足够，`MaxConcurrentReconciles: 8`。
- **不需要多集群 / 多 region**。API 保留 `region` 字段但不实现路由。
- **不需要 gRPC**（§07）。JSON 序列化开销在这个量级完全无关紧要。
- **容量估算**：1000 Pod × (250m / 512Mi request) ≈ 250 core / 500Gi 稳态占用；加预热冗余按 280 core / 560Gi 规划节点池。

### 0.4 本期不做

跨节点迁移、快照 / fork、多集群联邦、浏览器 / 桌面 / GPU、gVisor（见 §03.5，字段预留）。

---

## 1. 总体架构

```
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ Python SDK   │  │   Go SDK     │  │ Console (Web)│
└──────┬───────┘  └──────┬───────┘  └──────┬───────┘
       │  HTTPS  Authorization: Bearer     │  OIDC session
       └───────────────┬───────────────────┘
                       ▼
        ┌──────────────────────────────┐
        │     jdix-apiserver (Go)      │   无状态，多副本
        │  认证 · 配额 · 镜像准入        │   HTTP/JSON + WS
        │  管理平台后端 · 审计 · 计量     │
        └───┬──────────────────┬───────┘
            │ Postgres         │ 创建 Sandbox CR / watch status
            ▼                  ▼
   ┌────────────────┐   ┌──────────────────────────────┐
   │  租户/Key/用量  │   │  Kubernetes API              │
   │  模板/审计日志  │   │  CRD: Sandbox                │
   └────────────────┘   │       SandboxTemplate        │
                        │       SandboxPool            │
                        └───────────┬──────────────────┘
                                    ▼
                     ┌──────────────────────────┐
                     │    jdix-controller       │  Sandbox 绑定
                     │   (controller-runtime)   │  池供给 · 滚动 · GC
                     └───────────┬──────────────┘
                                 │ Pod CAS / create / delete
                                 │ HTTP POST /internal/v1/bind
                                 ▼
   ┌────────────────────────── Sandbox Pod ───────────────────────────┐
   │ initContainer: jdix-installer → 拷 execd/init/bwrap 到 emptyDir   │
   │ volumes: 模板声明的 CSI / NFS 卷（ROX 或 RWX）                     │
   │ container: 模板镜像（平台预置 或 租户自定义）                        │
   │   PID 1: jdix-execd  ── HTTP over unix socket ──►  bwrap ns       │
   │            │                                  └─ PID 1: jdix-init│
   │            │                                       └─ 用户进程    │
   │            └── :8080 数据面 HTTP/WS   :8081 控制面 HTTP/JSON       │
   └───────────────────────────────────────────────────────────────────┘
                                 ▲
                                 │ 直连 Pod IP（无 Service）
                     ┌───────────┴──────────────┐
                     │    jdix-gateway (Go)     │
                     └──────────────────────────┘
```

| 组件 | 语言 | 职责 |
|---|---|---|
| `jdix-apiserver` | Go | REST API、API Key 认证、配额、镜像准入、管理平台后端（embed Console） |
| `jdix-controller` | Go / controller-runtime | Sandbox 绑定、SandboxPool 供给、模板滚动、TTL 与兜底 GC |
| `jdix-execd` | Go | Pod 内守护进程，容器 PID 1；生成并 exec bwrap、暴露数据面 |
| `jdix-init` | Go（同二进制不同 argv） | bwrap namespace 内 PID 1；施加 spec 挂载、fork 用户进程、回收僵尸 |
| `jdix-gateway` | Go | 数据面反代 + 用户端口 `https://{sbx}-{port}.sbx.example.com` 路由 |
| `jdix-console` | React / Vite | 管理平台前端，embed 进 apiserver 二进制 |
| `jdixctl` | Go | CLI |

---

## 2. 核心决策

### D1 · 引入 `Sandbox` CRD，绑定放进 controller

```yaml
apiVersion: sandbox.jdix.io/v1alpha1
kind: Sandbox
metadata:
  name: sbx-01j8xk7m2q
  namespace: tenant-abc                  # 每租户一个 namespace
spec:
  templateRef: { name: py312-small }
  ttlSeconds: 1800
  filesystem:                            # ← bwrap 的输入，见 §04
    workspace: { path: /workspace, sizeLimit: 2Gi }
    mounts:
      - path: /data/corpus
        source: { volume: corpus, subPath: 2026-09 }
        readOnly: true
    allowSystemPaths: [/usr, /bin, /sbin, /lib, /lib64, /etc/ssl]
    hide: [/etc/hosts]
  env:
    - { name: RUN_ID, value: "42" }
  secretRefs: [ { name: hf-token } ]     # execd 内存注入，不进 Pod spec
  metadata: { user: u_9, job: eval-3 }
status:
  phase: Pending | Binding | Running | Expired | Failed | Terminating
  podName / podIP / nodeName
  isolationTier: userns
  coldStart: false
  endpoint: https://sbx-01j8xk7m2q.sbx.example.com
  boundAt / expiresAt
  conditions: [ { type: Ready, status: "True", … } ]
```

**绑定由 controller 独占**，apiserver 只创建 CR 并 watch status。不走"apiserver 自己抢 Pod"的快路径——那会让两个组件同时写 Pod 的 state label，制造 split-brain。

**这一跳的耗时预算**：

```
apiserver 创建 CR            10–25ms   (etcd 写)
informer 事件 → reconcile     5–20ms   (长连接内分发，非网络往返)
挑 idle Pod + CAS Update     10–30ms   (etcd 写)
execd Bind (HTTP) + bwrap    30–100ms  (§04)
status 写回 + apiserver 感知  10–25ms
────────────────────────────────────
合计                          65–200ms   ← 300ms 目标内
```

**必做的配套**：

- controller 单独一个 workqueue 处理 `phase: Pending`，与池供给、GC 队列隔离，避免供给风暴饿死绑定。
- `MaxConcurrentReconciles: 8`。
- apiserver 用 watch 而非轮询等待 `phase: Running`，8s 超时后返回 `pending` 让 SDK 拉取。
- Sandbox CR 打 finalizer，保证删 CR 必删 Pod。
- 绑定成功时把 Pod 的 ownerReference 从 SandboxPool **改挂到 Sandbox**，Pod 生命周期从此随 Sandbox 走。

### D2 · 沙箱 Pod 不建 Service

gateway 通过 informer cache 拿 `sandbox-id → PodIP` 本地路由，省掉 Endpoint 传播延迟（100ms~1s）。代价：gateway 需与 Pod 网络互通；用 `sandbox-id` 而非 IP 做主键，informer 删除事件立即失效缓存。

### D3 · 绑定用 Kubernetes 乐观锁

```
candidates = idle pods where template-hash == want AND tier >= minTier   // 随机打散
for pod in candidates:
    pod.labels[state] = "bound"; pod.labels[sandbox-id] = sbx.Name
    pod.ownerReferences = [sbx]
    if err := Update(pod); conflict { continue }
    return pod
// 无候选 → 冷启动：Create Pod，watch Ready
```

### D4 · 全链路 HTTP / WebSocket，不用 gRPC

见 §07。所有跨进程通信——SDK↔apiserver、apiserver↔gateway、controller↔execd、execd↔jdix-init——统一 HTTP/JSON，流式一律 WebSocket。

---

## 3. bubblewrap 隔离

### 3.1 这一层防什么

1. 不得读 `/var/run/secrets/kubernetes.io/serviceaccount/`。
2. 不得读写 `/opt/jdix` 下的 execd 二进制、控制面 token、gateway 凭据。
3. 不得看到 `/proc/1`（execd）的 cmdline / environ / fd。
4. 只能看到 `spec.filesystem` 声明的路径；写入限于 `/workspace` 与显式 rw 挂载点。
5. 用户命令以非 root、非 Pod 主 uid 运行。

### 3.2 三档隔离

| Tier | 内核前提 | bwrap 用法 | 说明 |
|---|---|---|---|
| **A `userns`** | 允许 unprivileged user namespace | `--unshare-user --unshare-pid --unshare-ipc --unshare-uts --unshare-cgroup` | 目标状态。容器无需额外 capability |
| **B `capadmin`** | 容器可加 `CAP_SYS_ADMIN` | 同上但不 `--unshare-user` | 强，但必须配 seccomp + AppArmor/SELinux |
| **C `chroot`** | 仅 `CAP_SYS_CHROOT` 或更少 | 不用 bwrap：chroot + setuid/setgid + 0700 + RLIMIT | 弱。**且不支持 spec 声明的动态挂载**，见 §04.4 |

Tier C 下第 1、2 条改由"物理不存在"保证：Pod 不挂 SA token，execd 在 Bind 前把 `/opt/jdix` 敏感文件改成 root 属主 `0600`，沙箱以 uid 1000 运行。

**永远不用 `--unshare-net`**——沙箱需要出网，网络隔离由 NetworkPolicy 负责（§08）。

### 3.3 Tier A 的 bwrap 命令（由 Sandbox spec 生成）

```sh
exec bwrap \
  --unshare-user --unshare-ipc --unshare-pid --unshare-uts --unshare-cgroup \
  --uid 1000 --gid 1000 \
  `# ── 静态骨架：由 SandboxTemplate 决定，与 sandbox 无关 ──` \
  --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind /sbin /sbin \
  --ro-bind /lib /lib --ro-bind-try /lib64 /lib64 \
  --ro-bind /etc/ssl /etc/ssl \
  --ro-bind /opt/jdix/skel/passwd      /etc/passwd \
  --ro-bind /opt/jdix/skel/group       /etc/group \
  --ro-bind /opt/jdix/skel/resolv.conf /etc/resolv.conf \
  --proc /proc --dev /dev \
  --tmpfs /tmp --tmpfs /run --tmpfs /home \
  --ro-bind /opt/jdix/bin/jdix-init /jdix-init \
  --bind /var/lib/jdix/ipc /run/jdix \
  `# ── 动态部分：由 spec.filesystem 生成 ──` \
  --bind    /var/lib/jdix/workspace          /workspace \
  --ro-bind /var/lib/jdix/vol/corpus/2026-09 /data/corpus \
  --tmpfs /etc/hosts.d  `# hide 用空 tmpfs 覆盖` \
  --chdir /workspace --hostname sandbox \
  --die-with-parent --new-session \
  -- /jdix-init
```

### 3.4 生成 bwrap 参数的安全规则

spec 里的路径来自租户，是**不可信输入**。生成器必须：

- 目标路径 `filepath.Clean` 后必须是绝对路径、不含 `..`，不得覆盖 `/proc`、`/dev`、`/opt/jdix`、`/run/jdix` 及系统只读路径。冲突直接拒绝，不做静默覆盖。
- 源路径必须落在模板声明的 volume 内（`source.volume` + `subPath`），subPath 解析后仍在该 volume 内（`openat2(RESOLVE_BENEATH)` 校验，防 symlink 逃逸）。
- 挂载点数量上限（建议 32）。
- `allowSystemPaths` 只能取平台白名单子集，不能新增。

这是继文件 API 之后第二个最容易写出漏洞的地方，需要专门的单元测试 + fuzz。

### 3.5 不使用 gVisor：隔离天花板与补偿措施

**明确记录已接受的风险。** 自定义镜像 + 同节点混跑 + 不用 gVisor，三者叠加意味着：**任意租户提供的任意二进制，与其他租户的沙箱共享同一个内核，中间只隔着 user namespace。**

一次内核提权漏洞就能跨越租户边界。因此：

**① 产品层面必须明说。** 不对外承诺"跨租户内核级隔离"。文档与合同里的措辞应是"命名空间级隔离"，而不是"虚拟机级隔离"。处理他人机密数据或运行明确不可信代码的场景，需要单独评估。

**② 平台强制的硬性下限**（不是模板可选项）：

- 使用租户自定义镜像的模板，`minIsolationTier` 强制 `userns`。集群若探不出 Tier A，自定义镜像功能整体关闭。
- `securityContext`：`runAsNonRoot: true`、`allowPrivilegeEscalation: false`、`capabilities.drop: [ALL]`、`readOnlyRootFilesystem` 尽可能开。
- 默认 deny-all egress，禁沙箱互访（§08）。

**③ seccomp 只用 `RuntimeDefault`，不做自定义 profile。**

自定义 profile 需要先回答"愿意为收紧攻击面付出多少兼容性代价"，而租户自带镜像意味着这个代价无法预估。本期不做这个投入。

唯一要做的是**在 podTemplate 里显式写上** `securityContext.seccompProfile.type: RuntimeDefault`。Kubernetes 在未开启 `SeccompDefault` feature gate 时默认是 `Unconfined`，不写等于完全没有 seccomp——那和"不做自定义 profile"是两回事，是一个字段的事。bwrap 的 `--seccomp` 也不用（§3.3 的命令里已去掉）。

**④ 节点内核加固**（Ansible / node bootstrap 里落地，P1）：

```
kernel.unprivileged_bpf_disabled = 1
kernel.yama.ptrace_scope         = 1
vm.unprivileged_userfaultfd      = 0
kernel.dmesg_restrict            = 1
```

配 AppArmor 或 SELinux profile；建立内核 CVE 订阅与打补丁的运维流程（这一条是流程不是代码，但同等重要）。

**⑤ 留钩子不留实现。** `SandboxTemplate.spec.runtimeClassName` 字段保留但默认为空。将来要接 gVisor 或 Kata，只需要建节点池 + 填这个字段，不用改架构。零成本的可选项，值得现在就留。

### 3.6 先探测

`hack/probe-isolation.sh` 在目标节点实测 unshare、bwrap Tier A/B、chroot、cgroup v2 委派，并检查 SA token 是否挂载、云元数据是否可达，用退出码（0/1/2/3）报出档位。同一逻辑内置进 execd 作运行时探测。

```sh
kubectl run jdix-probe --rm -it --image=debian:12 --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<node>"}}}' \
  -- bash -c "$(cat hack/probe-isolation.sh)"
```

---

## 4. bwrap 与预热池的时序

### 4.1 为什么必须在 Bind 之后施加

隔离的目录集合写在 `Sandbox.spec.filesystem` 里，预热时不存在，namespace 无法提前建完。**这是逻辑约束，不是性能取舍。**

基线实现：预热期把 Pod 拉起、卷挂好、execd 起好、tier 探测完；Bind 时 execd 读 Sandbox spec，生成 bwrap 参数，一次性 exec。

### 4.2 预热期承担的成本

静态骨架由 **SandboxTemplate** 决定，与具体 sandbox 无关，因此仍可预热：

| 预热期动作 | 收益 |
|---|---|
| 调度 + 镜像拉取 + 容器启动 | 1~4s |
| **CSI 卷 attach + mount** | **3~30s，见 §05.4——这是引入 volume 后最大的一头** |
| isolation tier 探测 | ~10ms；失败的 Pod 不进池（§04.5） |
| execd 启动 + 就绪 | ~5ms |
| 预读解释器与 .so（page cache 预热） | 首条命令省 50~300ms 冷 IO |

Bind 之后剩下：生成 bwrap 参数 → exec bwrap → jdix-init 就绪 → 注入 env/secret → 填充 workspace → 开数据面。**30~100ms**，取决于挂载点数量。

### 4.3 两阶段优化（P1，flag 后面）

v0.2 时这个优化的可行性存疑，因为不确定 volume 从哪来。**现在 volume 是 Pod 级的 CSI/NFS 卷、在 Pod 创建时就挂好，所以两阶段完全可行**：

1. **预热期**：execd 启动 bwrap，挂静态骨架 + `--tmpfs /workspace` + 一个隐藏的暂存绑定 `--bind /var/lib/jdix/vol /run/jdix/stage`（把整个卷根挂进去），在里面跑 `jdix-init` 待命。
2. **Bind 时**：execd 通过 unix socket 把 spec 下发给 jdix-init。jdix-init 在**自己的 user namespace 内是 root、持有该 ns 内的 `CAP_SYS_ADMIN`**，可以对自己的 mount namespace 直接调 `mount(MS_BIND)`：从 `/run/jdix/stage/<vol>/<subPath>` 绑到目标路径，需要只读的再 `mount(MS_BIND|MS_REMOUNT|MS_RDONLY)`。全部完成后 `umount2("/run/jdix/stage", MNT_DETACH)` 并 rmdir，抹掉暂存视图。

省下 exec bwrap 与静态挂载的 20~80ms。

**约束**：

- 源必须是模板已声明 volume 的子路径（现在这正是 spec 的定义，天然满足）。
- 只在 Tier A / B 可用；Tier C 下 jdix-init 无 mount 权限。
- **暂存视图的抹除必须可靠**——若 `umount2` 失败就必须让整个 Bind 失败并销毁 Pod，绝不能带着"能看到整个卷根"的 namespace 交给用户。这是两阶段方案唯一的新增风险点，要有专门测试。

**建议**：MVP 走基线（§04.1），M3 阶段实测 bind 五段耗时分解，只有 bwrap 那段真的占到 P99 显著比例才开优化。两条路共用 jdix-init 里同一份"施加 spec 挂载"的代码，差别只是何时 exec bwrap，所以这段从第一天就写成独立可测的模块。

### 4.4 Tier C 的能力缺口

没有 mount namespace，`spec.filesystem.mounts` 无法实现：

- 创建时校验：模板 tier 为 `chroot` 且 spec 声明了 `mounts` → 返回 400 并说明原因。
- `workspace` 仍可用（chroot 到工作目录）。
- Console 健康页对 Tier C 节点给明确告警。

不做"用拷贝模拟 bind mount"的兜底——语义不同（写入不回流、大目录拷贝耗时不可控），静默降级比报错更危险。

### 4.5 探测失败的 Pod 不进池

tier 探测在预热期完成，结果写入 Pod annotation 并参与 readinessProbe。探出的 tier 低于模板 `minIsolationTier` 就不进 idle 池，controller 视为失败并在别的节点重建（配 node anti-affinity 避开该节点）。故障因此从**用户请求路径**转移到**后台补给路径**。

---

## 5. 预热池与数据卷

```yaml
apiVersion: sandbox.jdix.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: py312-small
  namespace: tenant-abc              # 租户自定义模板；平台模板在 jdix-system
spec:
  minIsolationTier: userns
  runtimeClassName: ""               # 预留给将来的 gVisor / Kata，默认空
  defaultTTLSeconds: 1800
  maxTTLSeconds: 14400
  image:
    ref: registry.internal/jdix/py312@sha256:9a1f…   # 创建时 tag 解析为 digest
    pullSecretRef: { name: tenant-abc-regcred }
  filesystemDefaults:
    workspace: { sizeLimit: 2Gi }
    allowSystemPaths: [/usr, /bin, /sbin, /lib, /lib64, /etc/ssl]
  volumes:                           # sandbox spec 只能挂这里面的子路径
    - name: corpus
      claimName: corpus-rox          # 已存在的 PVC，必须 ROX 或 RWX（§05.4）
      mountPath: /var/lib/jdix/vol/corpus
      readOnly: true
  network:
    egress: restricted
    denyCIDRs: ["169.254.169.254/32", "10.0.0.0/8"]
  podTemplate:
    spec:
      automountServiceAccountToken: false
      containers:
        - name: main
          resources:
            requests: { cpu: 250m, memory: 512Mi }
            limits:   { cpu: "2",  memory: 2Gi }
---
apiVersion: sandbox.jdix.io/v1alpha1
kind: SandboxPool
metadata: { name: py312-small, namespace: tenant-abc }
spec:
  templateRef: { name: py312-small }
  replicas: 12                       # 目标 idle 数 —— 由平台管理员设置（§05.6）
  minReplicas: 2                     # 有卷模板下限抬到 3
  maxReplicas: 60
  autoscale: { targetIdleSeconds: 60 }
  notReadyTimeout: 300s               # 有卷模板 600s，见 §05.4
  idleTTLSeconds: 3600
  maxSurge: 3
```

### 5.1 Reconcile

```
desired = clamp(spec.replicas 或 autoscale 计算值, min, max)
current = count(pods where state=idle AND template-hash=current)
stale   = pods where state=idle AND template-hash != current

len(stale) > 0     → 删 stale，每轮 ≤ maxSurge
current < desired  → 创建差额
current > desired  → 删多余 idle，优先最老

# 卡死回收
idle 且 age > notReadyTimeout 且 not Ready → delete   ← 无卷 300s / 有卷 600s，见 §05.4
idle 且 age > idleTTLSeconds         → delete
# TTL 与兜底 GC
Sandbox 且 now > expiresAt           → phase=Expired，删 Pod
bound Pod 无对应 Sandbox（孤儿）      → delete
```

### 5.2 池容量

- 稳态创建速率 ≈ 0.6/s（1000 并发 / 30min 平均寿命）。
- `targetIdleSeconds: 60` 即 idle ≈ 未来 60 秒的 p95 创建量。峰值 20/s 会算出 1200 —— 不合理，所以**必须给 `maxReplicas` 封顶**，突发峰值交给冷启动路径吸收。
- 全平台 idle 总量控制在 50~80 个 Pod，约占稳态容量的 5~8%。

### 5.3 两个必须避开的反模式

- **不要用 Deployment / ReplicaSet 管预热 Pod**。Bind 的本质是把 Pod 从池里摘走并改挂 owner，ReplicaSet 会立刻重建或误删已绑定的 Pod。用裸 Pod + 自己的 controller 计数。
- **不要把 idle Pod 的 request 设成 0**。会导致节点超卖，Bind 后用户负载 OOM。预热池就是用钱换延迟，request 必须如实填。

### 5.4 Volume 与 CSI 的约束

支持任意 CSI / NFS 之后有四条硬约束，都会直接影响能不能跑通：

**① 只允许 `ReadOnlyMany` / `ReadWriteMany`，不允许 `ReadWriteOnce`。**

预热池里有 N 个 idle Pod，它们必须同时挂同一个卷。RWO 卷同一时刻只能被一个节点挂载，与预热池模型天然冲突。处理方式：

- 模板准入时校验 PVC 的 `accessModes`，含 RWO 且不含 ROX/RWX → 直接拒绝并说明原因。
- 确有 RWO 需求的模板，强制 `SandboxPool.replicas: 0`（纯冷启动），并在 Console 上标注"此模板不支持预热"。

**② CSI attach + mount 进入冷启动关键路径，典型 3~30s，个别云盘更久。**

这直接击穿"冷启动 < 5s"的目标。所以：

- **有卷的模板必须配预热池**，否则 create 延迟无从谈起。这不是优化建议，是可用性前提。
- 冷启动 SLO 分两档：无卷模板 < 5s；有卷模板不承诺，只承诺"命中预热池 < 300ms"。
- `SandboxPool` 对有卷模板的 `minReplicas` 下限抬高（建议 ≥ 3），避免池被抽干后长时间无法补给。
- 池的"卡死回收"超时（§05.1 的 5m）对有卷模板要放宽，否则正在 attach 的 Pod 会被误杀，形成"删了又建、永远补不上"的死循环。**这是最容易踩的运维坑。**
- 既然不限制 driver（④），这个超时的默认值给宽：**有卷模板 `notReadyTimeout: 600s`，无卷模板 `300s`**。controller 采集 `csi_attach_duration` 直方图，Console 的健康页按模板展示 p99；管理员据此把默认值收到实测 p99 的 3 倍左右。**新模板第一次上池时不要手工调这个值**，先跑一周看数据。

**③ 与"预热成本平台承担"叠加，产生一条新规则。**

租户声明一个 CSI 卷 ⇒ 该模板必须有预热池 ⇒ 平台要为此常驻 N 个 Pod ⇒ **平台出钱**。也就是说，租户可以通过"加一个卷"单方面产生平台成本。

因此：**带 volume 的模板必须经平台管理员审批**才能生效（`admission_status: pending_review`）。Console 上做一个审批队列，管理员看到的是"该模板需要 N 个常驻 Pod，约合每月 X 元"。这是 §06.1 配额分配机制的一部分。

**④ 安全：不做 StorageClass 白名单，控制点放在 PVC 申请上。**

不限制 driver / StorageClass。真正承担安全职责的是下面三条，它们已经覆盖了白名单能挡的东西：

- **禁止 `hostPath`**，无条件。这不是白名单，是类别禁止——hostPath 能直接读节点文件系统，包括 kubelet 凭据和其他租户的数据。
- **模板只能引用已存在于租户自己 namespace 的 PVC**，不能内联 CSI 参数。这条是关键：某些 NFS driver 允许通过 `volumeAttributes` 指定任意 server + export path，内联等于开了任意挂载；而引用已有 PVC 就把这个能力收进了 PVC 的创建流程。
- **PVC 创建走独立申请流程**（§05.5），每个卷都经过平台审批。审批就是控制点，白名单在它之上是重复的一道闸。

卷统一挂在 Pod 的 `/var/lib/jdix/vol/<name>`，沙箱看到的是 bwrap 里 spec 声明的子路径——沙箱**永远看不到卷根**，只能看到 subPath 之下。

**代价**：不限制 driver 意味着 attach 耗时不可预估（不同 driver 从 2s 到分钟级都有），所以 `notReadyTimeout` 不能靠"已知 driver 的经验值"来设，只能**默认给宽 + 按实测收敛**（见 ② 与 §05.1）。

### 5.5 卷的生命周期：租户长期持有

卷是租户申请一次、长期持有的资源，不随 sandbox 或任务创建销毁。这让实现简单很多（不需要卷的编排、不需要 provisioning 队列），但有三件事必须配套：

- **申请流程**：租户在 Console 提交（名称、容量、accessModes、用途），平台审批后由 SRE 或自动化创建 PVC，落 `volumes` 表。审批这一步同时承担了 §05.4 ④ 的安全职责。
- **容量监控与告警**：长期持有意味着卷内容只增不减。需要按 PVC 采集使用率，到 80% 告警给租户、到 95% 告警给平台。没有这个，第一次"卷写满"会以"沙箱莫名其妙写文件失败"的形式暴露出来，很难查。
- **租户离场（offboarding）回收**：租户停用时，卷不会自己消失，也不该被自动删除（可能有需要保留的数据）。需要一个明确流程：标记 → 通知 → 冻结（改 ROX）→ 保留期 → 删除。**这条不做的话，几年后集群里会堆满没人认领的 PVC。**

两条语义要写进文档告诉租户：**并发写不由平台保证**——RWX 卷可以被同租户的多个沙箱同时挂载并写入，平台不做锁、不做冲突检测，需要一致性请自行在应用层处理；**卷不随沙箱销毁而清理**，沙箱写进卷里的东西会一直在。

### 5.6 预热池由平台分配，不由租户自助

既然成本平台承担，`SandboxPool` 的 `replicas` 就不能是租户可写字段，否则任何租户都能把平台预算刷爆。

- **RBAC**：租户对 `SandboxTemplate` 有读写权，对 `SandboxPool` 只有读权。写权在 `jdix-admin` 角色。
- **全局预热预算**：管理员在 Console 上按模板分配，分配总和不得超过预算，超出时 UI 直接拦住并显示剩余额度。初始值（写进 `pool_budget` 的默认配置，后续按实际调）：

  | 项 | 初始值 | 依据 |
  |---|---|---|
  | `total_idle_pods` | **64** | 约占 1000 并发稳态容量的 6.4%，落在 §05.2 建议的 50~80 区间中部 |
  | `total_cpu` | **16 core** | 64 × 250m request |
  | `total_memory` | **32 Gi** | 64 × 512Mi request |
  | 单租户 `max_idle_pods` 默认 | **16** | 单租户最多占掉 1/4 预算，保证至少能同时服务 4 个租户 |
  | 平台主力模板（python/node/go）分配 | **各 12** | 合计 36，占预算 56% |
  | 其他平台模板分配 | **各 4** | |
  | 租户自定义模板分配 | **0**（需申请） | §06.1 |

  剩余额度 = 64 − 已分配，用于吸收审批通过的新申请。分配到 90% 时 Console 给平台管理员告警。
- **租户侧的入口是"申请"**：模板详情页有"申请预热"按钮，填期望并发与理由，进管理员审批队列。批准即写 `SandboxPool`。
- `quotas.max_idle_pods` 保留，语义从"租户可买多少"变成"管理员最多能给这个租户分多少"，是容量控制而非计费。
- `usage_records.idle_pod_seconds` 保留，语义从"向租户计费"变成"**内部成本归因**"——用来回答"哪个租户的预热池吃掉了多少平台预算"，这是将来决定是否改成租户付费的数据基础。

---

## 6. 租户自定义镜像

### 6.1 可池化单元 = 模板；预热额度由平台分配

自定义镜像不可能进公共池——池里的 Pod 必须已经拉起了对应镜像。所以租户在自己的 namespace 创建 `SandboxTemplate`，指定自己的镜像和 pull secret。

但**是否给这个模板配预热池，由平台管理员决定**（§05.6）。默认 `replicas: 0`，纯冷启动。Console 上把权衡显式呈现给管理员，而不是租户：

> 模板 `tenant-abc/my-ml-env`：镜像 4.2GB，无卷。
> 预热 8 个实例 ≈ 2 core / 4Gi 常驻，占全局预算 10%。
> 预期收益：该租户 create P99 从 ~28s（大镜像冷拉）降到 ~200ms。

### 6.2 镜像准入

apiserver 创建/更新 `SandboxTemplate` 时同步执行，同时配 ValidatingAdmissionWebhook 兜住直接 `kubectl apply` 的路径。

| 检查 | 说明 | 优先级 |
|---|---|---|
| Registry 白名单 | 只允许平台 registry + 租户显式登记的 registry。禁止 `docker.io` 匿名拉取（限流会造成随机冷启动失败） | P0 |
| Tag → digest 解析 | 创建时解析并**固化 digest**，模板永远按 digest 拉，避免 tag 漂移导致池内新旧镜像混杂 | P0 |
| 镜像大小上限 | 建议 5GB。超大镜像会把冷启动拖到分钟级 | P0 |
| 平台路径冲突检查 | 镜像不得在 `/opt/jdix`、`/var/lib/jdix` 放东西（initContainer 会覆盖，但要提前报错而非运行时诡异失败） | P0 |
| **卷 accessModes 校验** | 见 §05.4 ①，含 RWO 直接拒绝或强制 `replicas: 0` | P0 |
| **带卷模板转人工审批** | 见 §05.4 ③ | P0 |
| 必需二进制检查 | 镜像内需有 `/bin/sh`；缺失则所有 exec 失败 | P1 |
| Cosign 签名校验 | 租户可选开启，开启后拒绝未签名镜像 | P1 |
| 漏洞扫描门禁 | Trivy/Grype 按严重级别阻断。异步扫描 + 阻断新建，不影响已有沙箱 | P2 |

### 6.3 冷启动被镜像拉取支配

对策按性价比排序：

1. **有池的模板走节点预拉取**：DaemonSet 或 image pre-puller 按 digest 常驻。最有效。
2. registry 与集群同区，配 pull-through cache。
3. digest + `imagePullPolicy: IfNotPresent`。
4. 告知租户：多阶段构建、瘦身、共享 base layer 直接换成更快的 create。

### 6.4 明确不做的方案

**"通用 base Pod + Bind 时注入租户镜像 rootfs"**——理论上能让任意镜像共享一个池，但需要在 Pod 内做镜像解包与 overlay 挂载，等于自己实现半个容器运行时，并把 execd 的权限需求推高到 `CAP_SYS_ADMIN`（与 §3.5 强制 Tier A 冲突）。收益不抵复杂度和安全代价，不做。

---

## 7. 通信协议：全链路 HTTP / WebSocket

### 7.1 决策与取舍

所有跨进程通信统一 HTTP/JSON，流式一律 WebSocket，**不引入 gRPC / protobuf**。

| 链路 | 协议 | 鉴权 |
|---|---|---|
| SDK / Console → apiserver | HTTPS + JSON | `Authorization: Bearer jdix_sk_…` / OIDC session |
| SDK → gateway → execd 数据面 | HTTPS + JSON，流式 WS | `Authorization: Bearer sbt_…`（gateway 与 execd 各校验一次） |
| controller → execd 控制面 | HTTP + JSON（集群内 `:8081`） | mTLS 或集群内 bearer token |
| execd → jdix-init | **HTTP over unix socket**（`/run/jdix/init.sock`） | 文件权限 0600 + 单向 |
| apiserver → k8s | client-go（本来就是 HTTP） | ServiceAccount |

**换来的**：一套工具链（OpenAPI 3.1 生成两个 SDK 与所有内部 client）、`curl` 直接可调试、无需 protoc / grpc-gateway / grpc-web、Console 的 Web 终端与 SDK 复用同一套 WS 协议、故障排查时抓包可读。

**放弃的**：gRPC 的双向流语义（用 WS 替代，够用）、强 schema 与代码级契约保证、更省的序列化。在 ≤1000 并发下，JSON 的 CPU 与带宽开销完全无关紧要。

**需要补回来的**：gRPC 自带的 schema 约束没了，所以 **OpenAPI spec 必须当作强契约**——CI 里跑 spec lint、breaking-change 检测（如 `oasdiff`）、以及跨 SDK 的 conformance test。这不是可选项，否则内部 API 会很快漂移。

### 7.2 WebSocket 帧协议

流式接口（exec / pty / file watch）用同一套帧格式，避免每个接口各发明一套：

```jsonc
// client → server
{ "t": "stdin",  "d": "<base64>" }
{ "t": "resize", "cols": 120, "rows": 40 }
{ "t": "signal", "sig": "SIGINT" }
{ "t": "ping" }

// server → client
{ "t": "stdout", "d": "<base64>", "seq": 128 }
{ "t": "stderr", "d": "<base64>", "seq": 129 }
{ "t": "exit",   "code": 0, "durationMs": 812 }
{ "t": "error",  "code": "TIMEOUT", "msg": "…" }
{ "t": "pong" }
```

- 二进制数据 base64，避免文本/二进制帧混用带来的代理兼容问题。
- `seq` 单调递增，供断线续读用（§12 P1）。
- 应用层 ping/pong（30s）而非只依赖 WS 控制帧——中间的 LB 常常不透传控制帧，这是长连接掉线最常见的原因。

### 7.3 execd 数据面 API

```
# 执行
POST   /v1/exec                  同步，返回 {exitCode, stdout, stderr, durationMs}
GET    /v1/exec/stream           升级 WS，双向流
GET    /v1/pty                   升级 WS，PTY 会话
GET    /v1/processes             列出沙箱内进程
DELETE /v1/processes/{pid}       发信号

# 文件
GET    /v1/files?path=…          下载（支持 Range）
PUT    /v1/files?path=…          上传（流式）
DELETE /v1/files?path=…
GET    /v1/files/list?path=…     列目录
POST   /v1/files/archive         打包目录为 tar.gz
POST   /v1/files/extract         上传 tar.gz 解包
GET    /v1/files/watch?path=…    升级 WS，inotify 事件流

# 端口
POST   /v1/ports                 声明可访问端口
GET    /v1/ports

# 生命周期
POST   /v1/keepalive             续期 TTL
GET    /v1/health                存活 + 资源用量快照
```

### 7.4 execd 控制面 API（集群内，`:8081`）

```
POST /internal/v1/bind      # controller 调用：下发 Sandbox spec，触发 bwrap
                            # body: {sandboxId, filesystem, env, secrets, ttl, token}
POST /internal/v1/unbind    # 优雅销毁
GET  /internal/v1/status    # tier 探测结果、bwrap 状态、资源用量
GET  /internal/v1/probe     # readinessProbe 用
```

`/internal/v1/bind` 是同步接口，返回即代表 bwrap 已就绪、数据面已开。controller 拿到 200 才写 `status.phase: Running`。

**路径约束**：文件 API 的 path 必须 `filepath.Clean` 后落在 `/workspace` 或 spec 声明的 rw 挂载点内，并拒绝 symlink 逃逸——`openat2(RESOLVE_BENEATH)`（Linux 5.6+），降级用 `O_NOFOLLOW` 逐段打开。**必须有专门的 fuzz 测试。**

---

## 8. 网络

### 用户端口暴露

`https://{sandbox-id}-{port}.sbx.example.com` → gateway 按子域名解析 → 直连 `PodIP:port`，泛域名证书。

**不要用路径前缀**（`/sandboxes/{id}/ports/{p}/`）——会破坏沙箱内 web app 的绝对路径资源引用。端口需显式声明才可访问：`POST /v1/ports {port: 8000, public: true}`，返回带签名的 URL。

### 出网控制

默认 deny-all egress，放行 DNS 与模板声明的 CIDR。域名级白名单需要 L7（P1）：egress sidecar 做透明代理 + SNI 检查，或用 Cilium FQDN policy。

**同节点混跑下必须补的两条**：

1. **封禁云元数据服务 `169.254.169.254/32` 与集群内网段**——不封的话沙箱可直接偷云凭据、横向打内网。
2. **禁止沙箱 Pod 之间互访**——NetworkPolicy 里 podSelector 排除同类，否则一个租户的沙箱能直接连另一个租户沙箱的 `:8080`。

---

## 9. 控制面 API 与数据模型

```
POST   /v1/sandboxes                 创建（支持 Idempotency-Key）
GET    /v1/sandboxes                 列表（按 label/state/tenant 过滤，游标分页）
GET    /v1/sandboxes/{id}
DELETE /v1/sandboxes/{id}
POST   /v1/sandboxes/{id}/keepalive  续期
GET    /v1/templates
POST   /v1/templates                 创建租户模板（触发镜像准入）
POST   /v1/templates/{n}/warmup-request   申请预热（进管理员审批队列）
GET    /v1/quota

# 管理端（OIDC + admin role）
GET    /v1/admin/warmup-requests
POST   /v1/admin/pools/{n}           分配预热额度
GET    /v1/admin/budget              全局预热预算与已分配量
```

创建响应：

```json
{
  "id":            "sbx_01J8XK...",
  "state":         "running",
  "template":      "py312-small",
  "isolationTier": "userns",
  "endpoint":      "https://sbx-01j8xk.sbx.example.com",
  "token":         "sbt_...",
  "expiresAt":     "2026-09-08T10:30:00Z",
  "coldStart":     false
}
```

### Postgres schema 要点

```sql
tenants(id, name, status, oidc_subject_domain, created_at)
projects(id, tenant_id, name)
users(id, tenant_id, oidc_sub, email, role, created_at)
api_keys(id, tenant_id, project_id, key_id, secret_hash, prefix,
         name, scopes jsonb, allowed_templates text[], ip_allowlist cidr[],
         expires_at, last_used_at, revoked_at, created_by)
templates(name, tenant_id, image_digest, image_ref, admission_status,
          scan_status, has_volumes bool, spec jsonb, visibility)
registries(id, tenant_id, host, credential_ref, verified_at)
volumes(id, tenant_id, pvc_name, storage_class, access_modes text[],
        capacity_bytes, used_bytes, used_at,          -- 容量监控，见 §05.5
        status,                                       -- active|frozen|pending_delete
        approved_by, approved_at, retention_until)    -- 长期持有 + offboarding
warmup_requests(id, tenant_id, template, requested_replicas, reason,
                status, reviewed_by, reviewed_at)     -- 预热申请队列
pool_budget(id, total_idle_pods, total_cpu, total_memory, updated_by)
  -- 初始值 64 / 16 core / 32Gi，见 §05.6
quotas(tenant_id, max_concurrent, max_cpu, max_memory,
       max_create_qps, max_sandbox_seconds_per_day, max_idle_pods)
sandboxes(id, tenant_id, project_id, api_key_id, template, node, pod_uid,
          state, cold_start, isolation_tier, created_at, ready_at,
          deleted_at, delete_reason, metadata jsonb)  -- 非事实源
usage_records(sandbox_id, tenant_id, sandbox_seconds, cpu_seconds,
              mem_gb_seconds, egress_bytes, idle_pod_seconds, recorded_at)
audit_logs(id, tenant_id, actor, action, target, request_id, ip, ua, at, detail jsonb)
```

`idle_pod_seconds` 与 `max_idle_pods` 的语义在 v0.3 变了：不再是向租户计费的依据，而是**内部成本归因**与**容量控制**（§05.6）。数据继续采，为将来若改成租户付费留下依据。

### API Key

- 格式 `jdix_sk_<keyid:12>_<secret:32>`。固定前缀便于日志脱敏与注册 GitHub secret scanning pattern。
- 存 `argon2id(secret)`；`key_id` 明文索引。创建后**只显示一次**。
- 传递优先 `Authorization: Bearer jdix_sk_…`，兼容 `X-Jdix-Api-Key` 头。
- 校验结果按 `key_id` 缓存 60s（含 revoked 状态），撤销时通过 Postgres `NOTIFY` 广播即时失效。
- Scope：`sandbox:create` / `sandbox:read` / `sandbox:delete` / `template:read` / `template:write` / `admin:*`，叠加 `allowed_templates`、`ip_allowlist`、`expires_at`。

---

## 10. 管理平台（Console）

SPA embed 进 apiserver 二进制。登录走 **OIDC**（Authorization Code + PKCE），与 API Key 认证链路完全分离。

- 首次登录按 email 域名或 IdP group claim 映射到 tenant，落 `users` 表；映射规则在平台管理员界面配置。
- 角色 `owner` / `admin` / `developer` / `viewer` + 平台侧 `platform-admin`；OIDC group claim 可自动授予。
- 会话用 HttpOnly + SameSite=Lax cookie，服务端存 session，支持强制登出。
- 需要 refresh token 轮换与 `id_token` 过期后的静默续期，否则长时间开着的 Console 会突然 401。
- 预留 SCIM（P2）做用户自动开通/停用。

| 模块 | 内容 | 可见角色 |
|---|---|---|
| 概览 | 并发沙箱数、池命中率、冷启动率、Bind P50/P99、失败率、tier 分布 | 全部 |
| API Key | 创建（选 scope/模板/IP 白名单/有效期）、列表、撤销、最近使用 | 租户 |
| 模板与镜像 | 编辑 SandboxTemplate、镜像准入状态与扫描结果、registry 登记、灰度发布 | 租户 |
| 卷 | 申请 PVC、查看已批准的卷及其 accessModes | 租户 / 审批在平台侧 |
| 预热申请 | 提交申请、看进度与结果 | 租户 |
| **预热预算** | **全局预算、按模板分配、剩余额度、审批队列** | **platform-admin** |
| 沙箱 | 列表与详情、Web 终端直连、文件浏览、强制销毁 | 租户 |
| 配额与用量 | 租户配额编辑、用量报表、**预热成本归因**、导出 | 租户读 / 平台写 |
| 审计 | 操作日志检索 | 全部 |
| 健康 | **节点 tier 探测矩阵**、CSI attach 耗时分布 | platform-admin |

最后一项是运维刚需。没有它，"某个新加的节点内核不对，导致 5% 请求走冷启动"和"某个 CSI driver attach 慢到把池抽干"这两类问题都会拖到客户投诉之后才开始查。

---

## 11. SDK

OpenAPI 3.1 是单一事实源：传输层生成、易用层手写。两个 SDK 用同一份 conformance test 保证行为一致（在没有 gRPC 强 schema 之后，这套测试的重要性上升，见 §07.1）。

### Python

```python
from jdix import Sandbox, Client

with Sandbox.create(template="py312-small", ttl=600) as sbx:
    r = sbx.run("pip install requests && python -c 'import requests; print(requests.__version__)'")
    print(r.exit_code, r.stdout)

    sbx.files.write("/workspace/main.py", "print('hi')")
    for chunk in sbx.run_stream("python main.py"):
        print(chunk.data, end="")

    url = sbx.expose(8000)

# 带自定义文件夹限制
sbx = Sandbox.create(
    template="py312-small",
    mounts=[{"path": "/data/corpus", "volume": "corpus",
             "sub_path": "2026-09", "read_only": True}],
)
```

`with` 退出即 delete；后台线程自动 keepalive；`run` 默认带超时与输出上限；异常层次化（`SandboxTimeout` / `QuotaExceeded` / `SandboxDied` / `ImageAdmissionError` / `MountRejected`）；提供 `AsyncSandbox`。

### Go

```go
c, _ := jdix.NewClient(jdix.WithAPIKey(os.Getenv("JDIX_API_KEY")))
sbx, err := c.Create(ctx, jdix.CreateOpts{
    Template: "py312-small",
    TTL:      10 * time.Minute,
    Mounts: []jdix.Mount{{
        Path: "/data/corpus", Volume: "corpus",
        SubPath: "2026-09", ReadOnly: true,
    }},
})
defer sbx.Close(ctx)

res, _ := sbx.Run(ctx, "go version")
fmt.Println(res.ExitCode, res.Stdout)

stream, _ := sbx.RunStream(ctx, "make build")
for ev := range stream.Events() { /* … */ }

f, _ := sbx.Open(ctx, "/workspace/out.bin")   // io.Reader，不是 []byte
io.Copy(dst, f)
```

一切带 `context.Context`；命令非零退出用返回值表达而非 `error`；文件走 `io.Reader/Writer` 支持大文件；幂等操作自动重试 + 指数退避；内置 OTel instrumentation；WS 用 `nhooyr.io/websocket` 或 `gorilla/websocket`，自带 30s 应用层 ping。

---

## 12. 建议补充的 Feature

### P0 · 不做会出事

1. **TTL + 心跳双保险**：execd 到点自杀、controller 兜底 GC、Sandbox finalizer 保证删 CR 必删 Pod。SDK 崩溃时 Pod 必须能自己消失——同类系统的第一号事故源。
2. **元数据服务与内网封禁 + 沙箱互访封禁**（§08）。
3. **显式设置 `seccompProfile: RuntimeDefault`**（§3.5 ③）。一个字段；不写等于 `Unconfined`。
4. **配额与限流**：并发数、CPU/内存总量、创建 QPS、每日 sandbox-seconds。超限 429 带 `Retry-After`。
5. **镜像与卷准入**：registry 白名单、digest 固化、大小上限、平台路径冲突、**卷 accessModes 校验**、**带卷模板转人工审批**。
6. **预热预算与 RBAC**（§05.6）：租户不可写 `SandboxPool`，全局预算硬上限。
7. **有卷模板的池超时放宽**（§05.4 ②）——否则正在 attach 的 Pod 被误杀，形成永远补不上的死循环。
8. **fork bomb 与磁盘炸弹防护**：cgroup v2 `pids.max`、emptyDir `sizeLimit`、`RLIMIT_FSIZE`、exec 输出上限。
9. **幂等创建**：`Idempotency-Key`，24h 内重放返回同一沙箱。
10. **路径逃逸防护 + fuzz（两处）**：文件 API 与 bwrap 参数生成器。
11. **OpenAPI 契约 CI**（§07.1）：spec lint + breaking-change 检测 + 跨 SDK conformance test。没有 gRPC 的 schema 约束之后，这是唯一防止内部 API 漂移的手段。
12. **优雅销毁**：SIGTERM 宽限 5s → SIGKILL → 删 Pod。

### P1 · 上量后必须有

13. **节点内核加固 sysctl + AppArmor/SELinux profile**（§3.5 ④）。
14. **镜像预拉取 DaemonSet**：自定义镜像场景下冷启动的最大杠杆。
15. **池自动伸缩**：按最近 5 分钟创建速率算 idle 数，配时段化基线与 `maxReplicas` 封顶。
16. **全链路可观测**：OTel 从 SDK → apiserver → controller → execd 打通。关键指标 `bind_duration_seconds`（按 §D1 五段分别打点）、`pool_hit_ratio`、`csi_attach_duration`、`image_pull_duration`、tier 分布。
17. **exec 会话可恢复**：`seq` + 服务端输出 ring buffer，WS 断线后 `?resume_from=<seq>` 续读（帧协议已预留，见 §7.2）。
18. **两阶段 bwrap 优化**（§4.3），仅当实测需要；含暂存视图抹除失败的强制销毁逻辑。
19. **模板灰度 · Secret 注入 · Web 终端 · 用量导出 · cosign 校验**。

### P2 · 差异化

20. **MCP Server**：把 sandbox 能力包成 MCP 工具，让 Agent 直接调用。当前生态最好的分发方式，成本低。
21. **命令录制与回放**（合规），模板级开关。
22. **`jdixctl` CLI**：`jdix run` / `jdix shell` / `jdix cp`。
23. **出网审计 · 漏洞扫描门禁 · SCIM · 多 region**。
24. **gVisor / Kata**：字段已预留（`runtimeClassName`），将来只需建节点池 + 填字段。

---

## 13. 实施路线

> 实现进度截至 2026-09-09：M0–M6 已完成并通过测试，代码在本仓库。
> Go 实现约 7.7k 行、测试约 3.7k 行（123 个测试与 fuzz 目标）；两个 SDK 各自可用。
> 详见 [README](../README.md)。

| 阶段 | 状态 | 交付 | 验收 |
|---|---|---|---|
| **M0** 探针 | ✅ 已完成 | `hack/probe-isolation.sh` 在目标集群跑通 | 确定实际可用 tier。**若探不出 Tier A，需先确认是否仍开放自定义镜像**（§3.5） |
| **M1** 数据面 | ✅ 已完成 | execd + jdix-init + bwrap 参数生成器（Tier A/C）+ WS 帧协议，本地 Docker 跑通 | 能按 spec 挂载并在隔离 ns 内跑命令读写文件；逃逸测试与 fuzz 全绿 |
| **M2** 控制面骨架 | ✅ 已完成 | apiserver + Sandbox CRD + controller（无池，直接建 Pod）+ Postgres + API Key | SDK 能 create/run/delete，冷启动路径打通 |
| **M3** 预热池 | ✅ 已完成 | SandboxTemplate/SandboxPool + Bind CAS + ownerRef 转移 | 命中时 P99 < 300ms；**产出 bind 五段耗时分解**，据此决定是否做 §4.3 |
| **M4** 网关与网络 | ✅ 已完成 | gateway 反代 + 端口暴露 + NetworkPolicy（含沙箱互访封禁） | 沙箱内起 web server 外部可访问；元数据服务与邻居沙箱均不可达 |
| **M5** 卷与自定义镜像 | ✅ 已完成 | CSI 卷支持 + 准入链路 + 预热预算与审批 + 预拉取 | 带卷模板可用且池不被误杀；恶意/超规镜像与 RWO 卷被拒且错误信息清晰 |
| **M6** SDK | ✅ 已完成 | Python + Go + conformance test + OpenAPI 契约 CI | 两个 SDK 通过同一套行为测试；spec breaking change 能在 CI 拦住 |
| **M7** 管理平台 | ⬜ 未开始 | Console 全模块 + OIDC + 预热预算界面 | 可 OIDC 登录、建 key、审批预热申请、看 tier 矩阵与 CSI 耗时、开 Web 终端 |
| **M8** 加固 | ⬜ 未开始 | P0 全部落地 + 压测 + 混沌测试 | 1000 并发稳定；杀 apiserver/controller 不泄漏 Pod |

---

## 14. 更名对照表

| 旧 | 新 |
|---|---|
| `jdi-sandbox` | `jdix-sandbox` |
| `jdi-apiserver` / `jdi-controller` / `jdi-execd` / `jdi-init` / `jdi-gateway` / `jdi-console` | `jdix-` 前缀同名 |
| `jdictl` | `jdixctl` |
| CRD group `sandbox.jdi.io` | `sandbox.jdix.io` |
| `/opt/jdi`、`/var/lib/jdi`、`/run/jdi` | `/opt/jdix`、`/var/lib/jdix`、`/run/jdix` |
| API Key `jdi_sk_…` | `jdix_sk_…` |
| 头 `X-JDI-Api-Key` | `Authorization: Bearer`（主）/ `X-Jdix-Api-Key`（兼容） |
| 环境变量 `JDI_API_KEY` | `JDIX_API_KEY` |
| Python 包 `jdi` / Go 包 `jdi` | `jdix` |

仓库目录仍是 `jdi-sandbox/`。改名需要重开会话：

```sh
mv ~/Projects/go/project/jdi-sandbox ~/Projects/go/project/jdix-sandbox
```

---

## 15. 实施中需要定的细节

三个待确认项已全部有答案（不做 StorageClass 白名单、卷长期持有、预算 64 idle Pod），**没有阻塞开工的问题**。以下是实施到对应阶段时再定即可的细节：

| 何时 | 要定的东西 |
|---|---|
| M5 | 卷申请表单的默认容量与容量上限；容量告警的通知渠道（邮件 / 企微 / Webhook） |
| M5 | 租户 offboarding 的保留期长度（§05.5），建议 90 天 |
| M7 | OIDC IdP 的具体端点与 group claim 名称；email 域名 → tenant 的映射规则 |
| M7 | 预热申请审批的通知渠道与 SLA（多久必须给答复） |
| M8 | 压测的目标形状：稳态 1000 并发，还是要测突发峰值？峰值多少 |
