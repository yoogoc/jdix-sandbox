#!/usr/bin/env bash
# jdix-sandbox 隔离能力探测
#
# 在目标集群的一个 Pod 内运行，判断该节点能支持哪一档隔离（Tier A/B/C）。
# 退出码：0=Tier A(userns)  1=Tier B(capadmin)  2=Tier C(chroot)  3=无任何隔离能力
#
# 用法：
#   kubectl run jdix-probe --rm -it --image=debian:12 --restart=Never \
#     --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<node>"}}}' \
#     -- bash -c "$(cat hack/probe-isolation.sh)"
#
# 想同时验证 Tier B，加上 capability：
#   --overrides='{"spec":{"containers":[{"name":"jdix-probe","image":"debian:12","stdin":true,"tty":true,
#     "securityContext":{"capabilities":{"add":["SYS_ADMIN"]}}}]}}'

set -uo pipefail

ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m✘\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
hdr()  { printf '\n\033[1m%s\033[0m\n' "$*"; }

need_bwrap() {
  command -v bwrap >/dev/null 2>&1 && return 0
  hdr "安装 bubblewrap"
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq bubblewrap >/dev/null 2>&1
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache bubblewrap >/dev/null 2>&1
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q bubblewrap >/dev/null 2>&1
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q bubblewrap >/dev/null 2>&1
  fi
  command -v bwrap >/dev/null 2>&1
}

hdr "0. 环境信息"
echo "  kernel:    $(uname -r)"
echo "  arch:      $(uname -m)"
echo "  distro:    $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || echo unknown)"
echo "  uid/gid:   $(id -u)/$(id -g)"
echo "  caps(eff): $(grep -E '^CapEff' /proc/self/status | awk '{print $2}')"
if command -v capsh >/dev/null 2>&1; then
  echo "  decoded:   $(capsh --decode="$(grep -E '^CapEff' /proc/self/status | awk '{print $2}')" 2>/dev/null | sed 's/^0x[0-9a-f]*=//')"
fi
echo "  cgroup:    $(stat -fc %T /sys/fs/cgroup 2>/dev/null)"

hdr "1. 内核 user namespace 开关"
UNPRIV_CLONE=$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null || echo "n/a")
MAX_USER_NS=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo "n/a")
echo "  kernel.unprivileged_userns_clone = $UNPRIV_CLONE  (n/a 表示该发行版没有此开关，通常即默认允许)"
echo "  user.max_user_namespaces         = $MAX_USER_NS"
[ "$MAX_USER_NS" = "0" ] && bad "max_user_namespaces=0，user namespace 被硬关闭"

hdr "2. unshare 能力"
USERNS_OK=0
if unshare --user --map-root-user true 2>/dev/null; then
  ok "unshare --user 成功（unprivileged user namespace 可用）"; USERNS_OK=1
else
  bad "unshare --user 失败: $(unshare --user --map-root-user true 2>&1 | head -1)"
fi
MOUNTNS_OK=0
if unshare --mount true 2>/dev/null; then ok "unshare --mount 成功（有 CAP_SYS_ADMIN）"; MOUNTNS_OK=1
else bad "unshare --mount 失败（无 CAP_SYS_ADMIN，符合默认容器预期）"; fi
if unshare --pid --fork true 2>/dev/null; then ok "unshare --pid 成功"; else bad "unshare --pid 失败"; fi

hdr "3. bubblewrap 实测"
TIER_A=0; TIER_B=0
if ! need_bwrap; then
  warn "无法安装 bubblewrap（可能无出网/无包管理器），跳过实测，仅按 unshare 结果推断"
else
  echo "  bwrap: $(bwrap --version 2>&1)"
  # Tier A: 完全 unprivileged
  if out=$(bwrap --unshare-user --unshare-pid --unshare-ipc --unshare-uts --unshare-cgroup \
             --uid 1000 --gid 1000 \
             --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind-try /lib /lib \
             --ro-bind-try /lib64 /lib64 --ro-bind-try /sbin /sbin \
             --proc /proc --dev /dev --tmpfs /tmp \
             --die-with-parent --new-session \
             -- /bin/sh -c 'echo TIERA:$(id -u):$(ls /proc | grep -c "^[0-9]*$")' 2>&1); then
    ok "Tier A 可用 → $out"; TIER_A=1
  else
    bad "Tier A 不可用: $(echo "$out" | head -2 | tr '\n' ' ')"
  fi
  # Tier B: 依赖 CAP_SYS_ADMIN，不 unshare user
  if out=$(bwrap --unshare-pid --unshare-ipc --unshare-uts \
             --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind-try /lib /lib \
             --ro-bind-try /lib64 /lib64 --ro-bind-try /sbin /sbin \
             --proc /proc --dev /dev --tmpfs /tmp \
             --die-with-parent --new-session \
             -- /bin/sh -c 'echo TIERB:$(id -u)' 2>&1); then
    ok "Tier B 可用 → $out"; TIER_B=1
  else
    bad "Tier B 不可用: $(echo "$out" | head -2 | tr '\n' ' ')"
  fi
fi

hdr "4. Tier C 降级前提"
CHROOT_OK=0
TD=$(mktemp -d); mkdir -p "$TD/bin" "$TD/lib" "$TD/lib64" "$TD/usr"
if chroot "$TD" /bin/true 2>/dev/null || [ "$(chroot / /bin/echo c-ok 2>/dev/null)" = "c-ok" ]; then
  ok "chroot 可用（有 CAP_SYS_CHROOT）"; CHROOT_OK=1
else
  bad "chroot 不可用: $(chroot / /bin/echo x 2>&1 | head -1)"
fi
rm -rf "$TD"
if [ -e /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
  warn "ServiceAccount token 已挂载 —— 沙箱模板务必设置 automountServiceAccountToken: false"
else
  ok "ServiceAccount token 未挂载"
fi
if command -v curl >/dev/null 2>&1; then
  if curl -s --max-time 2 -o /dev/null -w '' http://169.254.169.254/ 2>/dev/null; then
    warn "云元数据服务 169.254.169.254 可达 —— 必须用 NetworkPolicy 封禁"
  else
    ok "云元数据服务不可达"
  fi
fi

hdr "5. cgroup v2 委派（用于 pids.max / 内存子限额）"
if [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" = "cgroup2fs" ]; then
  ok "cgroup v2"
  if [ -w /sys/fs/cgroup/cgroup.subtree_control ]; then
    ok "subtree_control 可写（可创建子 cgroup 做进程数限制）"
  else
    warn "subtree_control 不可写，pids.max 需降级为 RLIMIT_NPROC"
  fi
else
  warn "非 cgroup v2（$(stat -fc %T /sys/fs/cgroup 2>/dev/null)），子 cgroup 限制不可用"
fi

hdr "结论"
if [ "$TIER_A" = 1 ] || { [ "$USERNS_OK" = 1 ] && ! command -v bwrap >/dev/null 2>&1; }; then
  printf '  \033[1;32mTier A (userns)\033[0m —— 最强隔离，容器无需任何额外 capability。推荐 minIsolationTier: userns\n'
  exit 0
elif [ "$TIER_B" = 1 ] || [ "$MOUNTNS_OK" = 1 ]; then
  printf '  \033[1;33mTier B (capadmin)\033[0m —— 需给容器 CAP_SYS_ADMIN。可用，但必须配 seccomp + AppArmor/SELinux 收敛逃逸面\n'
  printf '  建议同时排查为何 userns 不可用（多为 kernel.unprivileged_userns_clone=0 或 max_user_namespaces=0，可由节点 sysctl 打开）\n'
  exit 1
elif [ "$CHROOT_OK" = 1 ]; then
  printf '  \033[1;33mTier C (chroot)\033[0m —— 仅目录视图隔离。依赖 automountServiceAccountToken:false + uid 隔离 + 文件权限兜底\n'
  printf '  一次性 Pod 模型下可接受，但 /proc 无法重挂，沙箱进程能看到 execd。上生产前应推动节点开启 userns\n'
  exit 2
else
  printf '  \033[1;31m无可用隔离能力\033[0m —— 连 chroot 都做不到。需要联系集群管理员放开 CAP_SYS_CHROOT 或 userns\n'
  exit 3
fi
