#!/usr/bin/env bash
# Check whether this machine can debug jdix-sandbox locally, and say what to do
# about anything that is missing.
#
# The interesting question is the last one. Sandbox Pods deliberately have no
# Service, so jdix-controller and jdix-server's gateway role reach them by Pod
# IP. Whether a
# process on this host can route to the cluster's Pod CIDR decides how you
# debug: run those components here, or run them in the cluster and debug the
# rest from here.
set -uo pipefail

NS=${NS:-jdix-dev}
IMAGE=${IMAGE:-jdix/sandbox-base:dev}

ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m✘\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
hdr()  { printf '\n\033[1m%s\033[0m\n' "$*"; }

FAILED=0
need() {
  if command -v "$1" >/dev/null 2>&1; then ok "$1 $(${2:-true} 2>/dev/null | head -1)"
  else bad "$1 not found${3:+ — $3}"; FAILED=1; fi
}

hdr "1. toolchain"
need go        "go version"
need kubectl   "true"
need docker    "true"
command -v dlv >/dev/null && ok "dlv (delve) available" || warn "dlv not installed: go install github.com/go-delve/delve/cmd/dlv@latest"
command -v uv  >/dev/null && ok "uv available (python SDK tests)" || warn "uv not installed; the Python SDK tests need it"

hdr "2. cluster"
CTX=$(kubectl config current-context 2>/dev/null) || { bad "no kubectl context"; exit 1; }
SERVER=$(kubectl config view -o jsonpath="{.clusters[?(@.name=='$CTX')].cluster.server}" 2>/dev/null)
echo "  context: $CTX"
echo "  server : $SERVER"
case "$SERVER" in
  *127.0.0.1*|*localhost*|*192.168.*|*::1*) ok "looks like a local cluster" ;;
  *) bad "this is NOT a local cluster — switch context before debugging, or you will create Pods on someone else's cluster"; FAILED=1 ;;
esac
kubectl get --raw /readyz >/dev/null 2>&1 && ok "API server reachable" || { bad "API server unreachable"; FAILED=1; }

hdr "3. CRDs"
for c in sandboxes sandboxtemplates sandboxpools; do
  if kubectl get crd "$c.sandbox.jdix.io" >/dev/null 2>&1
  then ok "$c.sandbox.jdix.io"
  else bad "$c.sandbox.jdix.io missing — run: make install-crds"; FAILED=1; fi
done

hdr "4. sandbox image on the node"
# The kubelet reads containerd, not the Docker image store, so a locally built
# image is invisible until it is imported.
if kubectl get nodes -o json 2>/dev/null \
   | grep -q "\"${IMAGE%%:*}"; then
  ok "$IMAGE present on a node"
else
  bad "$IMAGE not on any node — run: make image load-image"; FAILED=1
fi

hdr "5. can this host reach Pod IPs?"
# This is what decides the debugging layout, so it is measured rather than
# assumed. A running Pod is needed; any Pod will do.
POD_IP=$(kubectl get pods -A -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.status.podIP}{"\n"}{end}' 2>/dev/null \
         | grep -v '^$' | grep -v '^10\.0\.0\.1$' | head -1)
if [ -z "$POD_IP" ]; then
  warn "no Running Pod to test against; start one and re-run"
else
  echo "  probing $POD_IP ..."
  # Any TCP response, including a refusal, proves the route exists. A timeout
  # means the packets are going nowhere.
  if timeout 4 bash -c "cat < /dev/null > /dev/tcp/$POD_IP/1" 2>/dev/null \
     || timeout 4 bash -c "cat < /dev/null > /dev/tcp/$POD_IP/10250" 2>/dev/null; then
    ok "Pod network is routable from this host"
    echo "     → run jdix-controller and jdix-server here (hack/dev/run.sh)"
  elif timeout 4 ping -c1 -W2 "$POD_IP" >/dev/null 2>&1; then
    ok "Pod IP answers ICMP; TCP probably works too"
    echo "     → run jdix-controller and jdix-server here (hack/dev/run.sh)"
  else
    warn "Pod network NOT routable from this host"
    echo "     → jdix-controller cannot bind sandboxes from here: binding is an"
    echo "       HTTP call to podIP:8081, and sandbox Pods have no Service."
    echo "     Options:"
    echo "       a) debug the data plane instead: hack/dev/sandbox.sh (uses port-forward)"
    echo "       b) run the controller in-cluster and debug apiserver/SDK from here"
    echo "       c) on OrbStack, enable host networking for Kubernetes in settings"
  fi
fi

hdr "6. node isolation tier"
warn "not measured here — apply config/samples/tier-a-pod.yaml and read its first log line"
echo "     A node that reports 'chroot' cannot run templates whose"
echo "     minIsolationTier is 'userns'; see DESIGN.md §3.5."

hdr "result"
[ "$FAILED" = 0 ] && ok "ready to debug" || bad "fix the items above first"
exit "$FAILED"
