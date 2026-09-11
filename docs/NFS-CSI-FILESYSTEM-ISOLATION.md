# NFS CSI 与沙盒文件系统隔离设计

状态：已实现，第一步与第二步验收已在 default 集群通过。日期：2026-09-11。验收记录见 §9。

## 1. 决策与边界

采用普通 Kubernetes Pod 挂载 NFS CSI 卷，在容器内使用非特权 bubblewrap 构造用户进程的文件系统视图。保留 bwrap 内部 user namespace 和 mount namespace；复用容器已有的 PID namespace 与受限 `/proc`，不再为文件系统隔离创建一套完整的嵌套容器环境。

一个 Pod 一生只服务一个 Sandbox，结束后销毁，预热池只保存尚未绑定的 Pod。保留现有的一次绑定原则。

职责如下：

| 层次 | 职责 |
| --- | --- |
| Kubernetes / 容器运行时 | 不同 Sandbox 的容器、网络和资源边界；通过 CSI 挂载存储 |
| execd | 受信任的平台控制面，验证绑定请求，创建文件系统视图，管理 Pod 内生命周期 |
| bubblewrap | 新根目录、挂载白名单、只读/读写属性、隐藏卷根及未授权子目录 |
| jdix-init | 在受限视图内执行命令及文件 API、回收后代进程 |
| NFS 服务端 | 文件持久化、共享语义、UID/GID/ACL 权限 |

Pod 不是虚拟机，各 Pod 仍共享节点内核。目录隐藏也不能替代 NFS 服务端授权；一旦整个容器边界被突破，容器已挂载的卷仍属于暴露范围。对不同租户的硬授权应使用各自的 export、服务端权限或独立 PVC。

## 2. 当前故障与偏离点

已检查 default 集群：节点为 Orb 的 k3s，内核为 7.0.14-orbstack，Kubernetes 为 v1.33.3+k3s1，containerd 为 2.0.5-k3s2。

`tenant-dev/corpus-py-4e0f62f8d412-5tkwx` 的 UID 与报错一致。它设置 `hostUsers: false`，使用 `tenant-dev/corpus` PVC，绑定 NFS CSI PV `jdix-corpus`，指向 `192.168.139.217:/srv/nfs/jdix/corpus`。

当前依赖链：

```text
minIsolationTier=userns
  -> bwrap 新建 user/PID namespace 与 procfs
  -> 容器需要 procMount=Unmasked
  -> Kubernetes 要求 hostUsers=false
  -> 运行时对 NFS 卷执行 MOUNT_ATTR_IDMAP
  -> NFS 客户端不支持，容器启动失败
```

NFS CSI 本身正常与否尚需独立验证；此报错已明确发生在容器运行时的 idmap 阶段。不能通过 chmod、fsGroup、NFS 版本或服务端 squash 参数解决这一阶段的错误。

## 3. 目标执行流程

```text
创建普通 Pod（hostUsers=true，默认 procMount）
  -> NFS CSI 挂载 PVC，保持现有 PV/PVC
  -> execd 以非 root 启动，进行实际文件系统能力探测
  -> Pod 进入空闲池
  -> Bind：验证请求和源路径，固定允许的目录对象
  -> bwrap 创建内部 user + mount namespace
  -> 建立系统骨架、workspace、选定 NFS 子目录、受限 /proc
  -> 丢弃 setup 权限，安装工作负载 seccomp，关闭源目录 FD
  -> jdix-init 启动、设置 subreaper、完成配置
  -> 验证视图与权限，发布 Sandbox Ready
  -> 到期、失败或取消：终止并销毁整个 Pod
```

NFS 挂载在进入内部 user namespace 前已经完成。bwrap 只对已挂载的 NFS 子目录做普通 bind mount，不设置 idmapped mount，也不在内部重新发起 NFS 挂载。

内部 user namespace 是非 root 进程取得其私有 mount namespace 中挂载权限的机制，同时用于阻止用户进程访问外层平台进程的受保护 `/proc` 入口。它仍是文件系统访问控制实现的一部分，不能随意删掉或替换为 chroot。

## 4. Pod 与 bwrap 设置

目标 Pod 配置：

- `hostUsers: true`，`procMount: Default`。
- 平台及用户工作负载保持非 root；不增加宿主 user namespace 中的 `CAP_SYS_ADMIN`，不使用 privileged。
- `allowPrivilegeEscalation: false`，capabilities drop ALL。
- 禁用 hostPID、hostIPC、hostNetwork、shareProcessNamespace；保持不挂载 ServiceAccount token。
- 保留现有资源限制与 NetworkPolicy；节点设置合理的 Pod PID 数量限制。
- 安装专用的启动阶段 seccomp profile，并检查节点 LSM 对非特权 user namespace 的支持。

目标 bwrap 参数变化：

| 当前行为 | 新模式 |
| --- | --- |
| `--unshare-user` | 保留，单 UID/GID 映射 |
| 私有 mount namespace、新根目录 | 保留 |
| `--unshare-pid`、`--as-pid-1` | 移除 |
| `--proc /proc` | 改为递归只读绑定容器已有 `/proc`，保留所有遮蔽子挂载 |
| IPC/UTS/cgroup namespace | 文件系统模式不再重复创建；删除依赖 `--unshare-uts` 的 hostname 设置 |
| 系统路径只读挂载、workspace、NFS 子目录 | 保留并加强源路径固定与只读检查 |
| 环境清理、会话分离、父进程退出联动 | 保留，但退出联动不能作为整棵进程树清理的唯一保证 |

这不是可直接复制执行的完整命令。尤其是递归只读 `/proc` 的 bind、devpts/PTY 初始化以及源路径 FD 传递，都必须在当前固定版本的 bwrap 中实测。

### 两阶段 seccomp

容器启动阶段的 profile 应在容器运行时基线之上精确允许 bwrap 实际使用的 namespace 与 mount 初始化调用。不能认为只放行 `unshare(CLONE_NEWUSER)` 就足够：还要根据实际版本检查 clone/clone3、bind/remount/pivot_root 等路径。禁止将全局 Unconfined 作为最终配置。

在执行 jdix-init/用户程序前，通过 bwrap 的 seccomp 支持叠加工作负载 profile，收回 namespace、mount、setns、ptrace、process_vm_* 等不需要的权限，保留 fork/exec、线程、PTY、网络和正常文件 IO。明确处理 clone 的 namespace flags；clone3 无法直接通过经典 seccomp 解引用参数结构，需决定禁用并验证 libc 的回退。叠加的 profile 只能进一步收紧，不能放宽外层 profile。

## 5. 必须成立的文件系统保证

### 5.1 `/proc` 与外层进程

共享 PID namespace 后，用户可以看到同容器中部分平台进程信息。这是本模式明确接受的行为，不承诺容器内 PID 隐藏。

但用户必须不能通过 `/proc/<outer-pid>/{root,cwd,fd,mem,environ}`、pidfd_getfd、ptrace 或 process_vm_* 取得外层文件视图或控制凭据。实现条件：

1. execd 和 bwrap 外层监控进程位于祖先 user namespace；所有租户代码只在内层 namespace 执行。Linux 对上述部分 `/proc` 入口使用 ptrace 访问检查，检查 user namespace 中的权限，不能只比较数字 UID。
2. execd 尽早设置不可 dump，并禁止把密钥放入 argv；清理启动子进程的环境和继承 FD。Go 多线程进程需验证实际进程防护效果。
3. 审核 bwrap 的外层监控进程、所有 Go 线程对应的 `/proc/<tid>` 和临时 helper。不可假定 bwrap 继承并一直保留不可 dump 状态：上游代码会在部分降权路径重新设置 dumpable。
4. `/proc` 只读并保留运行时遮蔽；用户不可移除遮蔽、重新挂载宽松 procfs 或加入祖先 namespace。不得给用户遗留祖先 namespace FD。
5. 阻止从用户进程向外层平台进程发送信号并不是本设计当前保证；同 UID、共享 PID 情况可能允许自毁该 Pod。平台进程死亡必须使 Pod 退出，不能影响其他 Pod 或释放未受控的后台进程。

只读 `/proc` 本身不能阻止通过 root/fd 魔术链接访问其他挂载，此项要以攻击测试结果为准。若该版本组合不能满足上述访问边界，新模式不得进入 Ready，不能以“Pod 内都可信”解释失败。

### 5.2 源路径不能在校验后被替换

当前 `Generate` 用 `path.Join(volumeRoot, subPath)` 传字符串给 bwrap。即使校验禁止 `..`，NFS 上的符号链接及并发 rename 仍可能改变路径解析结果。

改为受信任解析器从卷根 FD 开始，用 openat2 的 beneath/no-magiclinks/no-symlinks 约束取得 O_PATH FD，并固定挂载源对象。之后的挂载必须使用这个固定对象，不能转回原路径让 bwrap 重新解析。

FD 到挂载源的传递是原型必须解决的接口：检查固定版本 bwrap 是否可安全消费受控 `/proc/self/fd/N` 源并在租户执行前关闭这些 FD；若其路径规范化重新引入竞态，应使用一个范围明确的内部挂载 helper/补丁，不以 realpath 检查代替。整个过程不需要获取宿主 SYS_ADMIN。

第一版不允许租户控制的挂载点嵌套；只读卷遇到未知子挂载时拒绝，或在已验证的实现中递归施加只读，不能只保护顶层。

### 5.3 文件描述符与内部 API

进入用户程序前关闭卷根、未授权目录、旧根及 namespace FD，设置工作目录为允许的路径。必要的文件/PTY/IPC FD 逐项列出，而不是继承整个 execd 描述符集合。

jdix-init 的文件 API 继续在同一受限视图内运行；它不能为了方便回到 execd 的宽视图。内部 Unix socket 不凭“0600”就声称对同 UID 用户不可访问：配置接口必须一次性封闭并认证，用户可触达的数据接口不能提供扩大根目录或重新挂载的能力。

### 5.4 NFS 身份和授权

先支持一个明确的外层 UID/GID，例如现有的 1000:1000，内部身份映射回这个用户。服务端按实际 NFS 凭据执行权限检查，不能假定内部 root 获得 NFS root 权限。

首个版本不承诺任意 UID 范围或仅依赖 supplementalGroups 的 ACL 模型；测试并记录 root_squash、匿名用户、主 GID、补充组的实际行为。不自动递归 chown 共享数据。

子目录白名单是视图限制，不是服务端对象级授权。若其他客户端能把敏感文件硬链接到允许目录、移动目录内容或直接修改同一 export，它们仍可能改变可见内容。不同租户硬边界必须依赖独立的服务端授权范围。

## 6. 进程回收和生命周期

jdix-init 不再是 PID 1，必须在任何用户进程启动前执行 PR_SET_CHILD_SUBREAPER，沿用集中 wait4 的 Reaper，避免与 os/exec 的 Wait 竞争。处理 orphan 状态存储，不能让无限后台任务使 pending 映射无限增长。

execd 作为容器 PID 1 要有与 bwrap 主进程等待协调的回收机制；不能直接增加另一个无差别 wait4(-1) 循环。

TTL、unbind、启动失败、平台或 initd 异常均进入终止状态，关闭数据面并退出/删除 Pod，由运行时清理整个容器的任务。不能只杀 bwrap PID 或一个进程组：setsid、双重 fork 可以逃离进程组。已有 Pod 不返回预热池。

Bind 成功应等待 initd 的配置完成及视图自检，不以 socket 文件存在作为成功证据。

## 7. API 和代码改造

将“是否启用 Pod user namespace”和“文件系统视图能力”解耦，不继续用 userns > capadmin > chroot 的排名同时表示两者。

建议先在受控的新模板中增加明确的文件系统模式与 Pod 安全配置；字段名在实现时统一设计，例如 `filesystemIsolation: bwrap` 和独立的 `podUserNamespace`。这是提议字段，不是当前可应用 YAML。

旧模板的 `minIsolationTier: userns` 保持原含义；不能静默把它解释成新模式。新旧配置冲突时拒绝。通过新模板、新 hash 和新预热池迁移，已有绑定 Pod 按原策略运行到结束。

| 位置 | 修改内容 |
| --- | --- |
| `pkg/controller/podspec.go` | 解耦 hostUsers/procMount 与 bwrap 目录模式，生成专用 seccomp 配置 |
| `pkg/isolation/detect.go` | 使用与真实启动相同的目录模式探测，返回具体能力和失败原因 |
| `pkg/bwrap/generate.go`、`validate.go` | 精简 namespace 参数，受控 /proc bind，固定源目录，权限收紧 |
| `pkg/safepath` | 提供挂载源 FD 解析能力，禁止检查后重新按路径打开 |
| `pkg/execd/sandbox.go`、`cmd/jdix-execd` | 进程防护、FD/环境清理、生命周期与 PID 1 回收 |
| `cmd/jdix-init`、`pkg/initd` | subreaper、初始化接口封闭、用户代码启动前检查 |
| CRD / API / SDK / 模板控制器 | 独立能力契约、显式迁移及新的 Ready 条件 |
| `local/csi` | 保留 NFS PV/PVC，新增实验模板与兼容性测试 |

能力状态至少区分：外层卷挂载失败、内部 user namespace 不可用、proc 视图不安全、路径固定失败、只读保证失败、进程回收失败。普通 Pod 探测可通过并不证明它的每个 CSI 卷都支持该模式，必须结合实际卷检查。

## 8. 实施顺序和验收

第一步：用单独的临时 Pod，在当前 NFS CSI PVC 上验证精简 bwrap 参数。此阶段不替换现有池，不运行不可信租户代码。必须确认非 root、无新增宿主 capability、无 Pod idmap，同时能绑定真实 NFS 子目录与只读 proc。

第二步：实现 proc 防护、源 FD 固定、seccomp 和回收原型。验证以下项目后，才接受架构：

| 验证类别 | 必须达到的结果 |
| --- | --- |
| NFS 数据链路 | 两个普通 Pod 经 CSI 挂同一 PVC，允许的共享写入、rename、fsync 可观察 |
| 目录范围 | A 只见 A；B 只见 B；隐藏卷根、兄弟目录、平台文件不可达 |
| 只读 | RO 写入失败；remount、子挂载、链接和 FD 路径不能绕过 |
| proc 攻击 | 无法经所有外层进程/线程的 root、cwd、fd、mem、environ 获得宽视图；自有 /proc/self/fd 仅含允许 FD |
| 路径竞态 | symlink、magic-link、并发 rename 不会把挂载源换到授权范围外 |
| 权限 | 用户阶段 capability 清空、no_new_privs 生效；不能加入祖先 namespace 或建立绕过策略的新挂载 |
| 正常工作负载 | Python、线程/子进程、PTY、网络、文件 API、只读数据集可用 |
| 生命周期 | double-fork、setsid、僵尸进程、initd 崩溃、execd 崩溃和 TTL 后全部任务最终清理 |
| 凭据 | 主 UID/GID 与 root_squash 行为明确；组权限不满足时有明确错误 |

第三步：补齐 API/控制器，建立新的 NFS 测试池。新模式在所有必需探测通过后进入 Ready；失败不得自动改为 privileged、capadmin 或关闭目录隔离。

本设计中最需要先验证的是：已有遮蔽 procfs 的递归 bind、外层 bwrap 监控进程的 proc 访问保护、固定目录 FD 与 bwrap 的衔接。这三项未通过前，不把方案表述为已经解决。

## 9. 验收记录

环境：Orb 的 k3s，内核 7.0.14-orbstack，Kubernetes v1.33.3+k3s1，containerd 2.0.5-k3s2，**bubblewrap 0.8.0**，节点 arm64。卷为 `tenant-dev/corpus`（RWX，NFS CSI，`192.168.139.217:/srv/nfs/jdix/corpus`）。

Pod 由 `controller.BuildPod` 对一个 `filesystemIsolation: bwrap` 模板渲染后直接 apply，不经 controller，因此测的是真实 spec 而不是手抄的近似。execd 启动即报 `tier=filesystem`，理由 "pinned volume mounts and protected proc view available"。

### 第一步（§8）

| 项 | 结果 |
| --- | --- |
| 普通 Pod（`hostUsers:true`、`procMount` 默认）挂 NFS CSI 卷 | 通过，无 idmap 报错 |
| 精简 bwrap 参数启动 | 通过 |
| 非 root、无新增 capability | `NoNewPrivs: 1`，`CapEff: 0000000000000000` |

关键点：`--ro-bind /proc /proc` 取代 `--proc /proc` 之后 `procMount: Unmasked` 不再需要，§2 那条依赖链从根上断开。

### 第二步（§8 验收表）

| 验证类别 | 结果 |
| --- | --- |
| NFS 数据链路 | 两个沙箱同挂一个 PVC；写入、rename、fsync 在服务端可观察 |
| 目录范围 | A 只见 `a.txt`，B 只见 `b.txt`；互不可见；卷根、兄弟目录、`/opt/jdix`、`root-only.txt` 均不可达 |
| 只读 | 写 `/ro` 拒绝；`remount,rw` 失败；`/proc/self/fd` 路径写入拒绝；`mount --bind` 拒绝；跨挂载硬链接拒绝；文件 API 返回 403 `read_only` |
| proc 攻击 | 逐进程核对：`jdix-execd`(PID 1)、bwrap 监控进程、`jdix-init` 的 `root` 均不可读、可打开 fd 均为 0；`environ`/`cwd`/`mem`/`maps`/`task/*/fd` 全部拒绝；唯一可读的是沙箱自己的 `/proc/<self>`；`/proc` `remount,rw` 失败，遮蔽子挂载保留 |
| 路径竞态 | `pkg/safepath` 单测在 Linux 上实跑：指向外部与指向内部的符号链接都拒绝（只有 `RESOLVE_NO_SYMLINKS` 会拒后者）、traversal/绝对/非规范/NUL 全拒；固定后目录被 rename 掉包，fd 仍指向被校验过的那个目录 |
| 权限 | `no_new_privs=1`、`CapEff=0`；无法加入祖先 namespace，无法建立新挂载 |
| 正常工作负载 | Python 3.11、线程、子进程、DNS、`/dev/ptmx` 与 `pty.fork()` 往返、文件 API 读写、只读数据集读取均正常 |
| 生命周期 | `setsid` + 双重 fork 的逃逸进程重新挂到 `jdix-init`（subreaper 生效），不是 execd；僵尸数 0；unbind 后 Pod 进入 `Succeeded`，逃逸进程随容器销毁，节点上无残留 |
| 凭据 | 沙箱内 `uid=1000(sandbox)`；NFS 服务端看到的属主为 `1000:1000`；`chown 0:0` 被拒 |

### 仍未覆盖

- 探测与运行都只在单节点 arm64 上做过；amd64 的 profile 已生成但未在节点上跑过。
- 多线程 Go 进程作为外层监控进程的 proc 防护，测的是真实 execd（PID 1）与真实 bwrap 监控进程，但未针对 §5.1(3) 说的"所有 Go 线程对应的 `/proc/<tid>`"逐线程枚举。
- root_squash、匿名用户、补充组的行为只覆盖了主 UID/GID 这一种配置。
- 未做长时间压力运行，`pending` 上界与 subreaper 的长期行为只有单测覆盖。

## 10. 参考

- [Kubernetes user namespace 与 NFS/idmap 限制](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/#filesystem-support)
- [Kubernetes procMount 约束](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#managing-access-to-the-proc-filesystem)
- [Linux user namespace 中的 mount 权限](https://man7.org/linux/man-pages/man7/user_namespaces.7.html)
- [Linux mount namespace 的锁定挂载语义](https://man7.org/linux/man-pages/man7/mount_namespaces.7.html)
- [Linux proc_pid_root 访问检查](https://man7.org/linux/man-pages/man5/proc_pid_root.5.html)
- [Linux ptrace 的凭据与 user namespace 检查](https://man7.org/linux/man-pages/man2/ptrace.2.html)
- [bubblewrap 参数文档](https://github.com/containers/bubblewrap/blob/main/bwrap.xml)
- [bubblewrap 实现，含 dumpable 和降权路径](https://github.com/containers/bubblewrap/blob/main/bubblewrap.c)

引用的 bubblewrap main 分支用于理解实现，实际原型必须固定并记录部署的 bwrap 版本。本文没有改变运行中的集群、Pod 或现有业务代码。
