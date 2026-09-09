#!/usr/bin/env bash
# Show everything about one sandbox, or about the pool, in one screen.
#
# Debugging a bind means correlating a Sandbox object, a Pod, and execd's own
# view — which normally takes three commands and a lot of scrolling.
#
#   hack/dev/trace.sh                 # pool and sandbox overview
#   hack/dev/trace.sh sbx-abc123      # everything about one sandbox
#   hack/dev/trace.sh -f sbx-abc123   # follow its Pod's logs
set -euo pipefail

NS=${NS:-jdix-dev}
FOLLOW=0
[ "${1:-}" = "-f" ] && { FOLLOW=1; shift; }
SBX=${1:-}

hdr() { printf '\n\033[1m%s\033[0m\n' "$*"; }

if [ -z "$SBX" ]; then
  hdr "pools"
  kubectl -n "$NS" get sbxpool -o wide 2>/dev/null || echo "  (none)"
  hdr "templates"
  kubectl -n "$NS" get sbxtpl -o wide 2>/dev/null || echo "  (none)"
  hdr "sandboxes"
  kubectl -n "$NS" get sbx -o wide 2>/dev/null || echo "  (none)"
  hdr "pods by state"
  # The state label is what the bind flips, so it is the useful grouping.
  kubectl -n "$NS" get pods \
    -L sandbox.jdix.io/state,sandbox.jdix.io/isolation-tier,sandbox.jdix.io/sandbox-id \
    2>/dev/null || echo "  (none)"
  hdr "recent events"
  kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -12 || true
  exit 0
fi

hdr "Sandbox $SBX"
# The JSON goes through the environment rather than being interpolated into a
# quoted -c string: a shell single-quoted block cannot contain a single quote,
# and f-strings are full of them.
SBX_JSON=$(kubectl -n "$NS" get sbx "$SBX" -o json 2>/dev/null) python3 - <<'PY'
import json, os

raw = os.environ.get("SBX_JSON", "")
if not raw.strip():
    print("  no such Sandbox")
    raise SystemExit

d = json.loads(raw)
st, sp, meta = d.get("status", {}), d.get("spec", {}), d.get("metadata", {})
pod = "{} ({}) on {}".format(
    st.get("podName", "-"), st.get("podIP", "-"), st.get("nodeName", "-"))
rows = [
    ("phase",      st.get("phase", "-")),
    ("template",   sp.get("templateRef", "-")),
    ("pod",        pod),
    ("tier",       st.get("isolationTier", "-")),
    ("coldStart",  st.get("coldStart", False)),
    ("endpoint",   st.get("endpoint", "-")),
    ("expiresAt",  st.get("expiresAt", "-")),
    ("reason",     st.get("reason") or "-"),
    ("finalizers", ",".join(meta.get("finalizers", [])) or "-"),
]
for k, v in rows:
    print("  {:<11}: {}".format(k, v))

mounts = sp.get("filesystem", {}).get("mounts", [])
if mounts:
    print("  mounts     :")
    for m in mounts:
        src = m.get("source", {})
        print("    {} <- {}/{} ro={}".format(
            m.get("path"), src.get("volume"), src.get("subPath", ""), m.get("readOnly", False)))
PY
echo

POD=$(kubectl -n "$NS" get sbx "$SBX" -o jsonpath='{.status.podName}' 2>/dev/null || true)
if [ -z "$POD" ]; then
  # A Sandbox with no Pod yet is the interesting case: it is either waiting for
  # a warm one or was refused.
  echo "  no Pod bound yet — check the controller log and the pool's idle count"
  exit 0
fi

hdr "Pod $POD"
kubectl -n "$NS" get pod "$POD" -o wide 2>/dev/null
kubectl -n "$NS" get pod "$POD" -o jsonpath='
  labels : {.metadata.labels}
  hostUsers: {.spec.hostUsers}  seccomp: {.spec.securityContext.seccompProfile.type}  procMount: {.spec.containers[0].securityContext.procMount}
' 2>/dev/null
echo

hdr "execd log"
if [ "$FOLLOW" = 1 ]; then
  exec kubectl -n "$NS" logs -f "$POD"
fi
kubectl -n "$NS" logs "$POD" --tail=40 2>/dev/null || true
