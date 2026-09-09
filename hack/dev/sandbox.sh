#!/usr/bin/env bash
# Drive a sandbox Pod's data plane directly, without the controller.
#
# This is the fastest loop for anything in pkg/initd, pkg/execd or pkg/bwrap:
# it binds a sandbox over execd's control port and then talks to the data plane,
# all through kubectl port-forward — so it works even when this host cannot
# route to Pod IPs.
#
#   hack/dev/sandbox.sh                        # start a Pod, bind, open a shell
#   hack/dev/sandbox.sh -- 'ls -la /workspace' # run one command and exit
#   hack/dev/sandbox.sh --pod jdix-xyz         # use an existing Pod
#   hack/dev/sandbox.sh --keep                 # leave the Pod running afterwards
#   hack/dev/sandbox.sh --args                 # print the generated bwrap argv
set -euo pipefail

NS=${NS:-jdix-dev}
POD=""
KEEP=0
SHOW_ARGS=0
CMD=""
CTRL_PORT=${CTRL_PORT:-18181}
DATA_PORT=${DATA_PORT:-18180}
SBX_TOKEN=${SBX_TOKEN:-sbt-dev-$$}
# Each Pod carries its own control-plane credential; execd refuses to start
# without one, so a Pod this script creates gets one here.
CTL_TOKEN=${CTL_TOKEN:-jct-dev-$$}

while [ $# -gt 0 ]; do
  case "$1" in
    --pod)   POD=$2; shift 2 ;;
    --keep)  KEEP=1; shift ;;
    --args)  SHOW_ARGS=1; shift ;;
    --)      shift; CMD="$*"; break ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

CREATED=0
if [ -z "$POD" ]; then
  # Prefer an idle warm Pod if a controller has been supplying the pool; it is
  # already running and already measured.
  POD=$(kubectl -n "$NS" get pods -l sandbox.jdix.io/state=idle \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -n "$POD" ]; then
    log "using warm Pod $POD"
  else
    POD="jdix-dev-$$"
    CREATED=1
    log "no warm Pod; starting $POD"
    # The three concessions the userns tier needs; see config/samples/tier-a-pod.yaml
    # for why each one is there.
    kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  labels: { app.kubernetes.io/managed-by: jdix-dev }
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  hostUsers: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile: { type: Unconfined }
  containers:
    - name: sandbox
      image: ${IMAGE:-jdix/sandbox-base:dev}
      imagePullPolicy: Never
      args: ["--log-level=debug"]
      env:
        - { name: JDIX_CONTROL_TOKEN, value: "${CTL_TOKEN}" }
      securityContext:
        allowPrivilegeEscalation: false
        procMount: Unmasked
        capabilities: { drop: ["ALL"] }
      readinessProbe:
        httpGet: { path: /internal/v1/probe, port: 8081 }
        initialDelaySeconds: 1
        periodSeconds: 2
        failureThreshold: 60
YAML
    kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null \
      || die "Pod $POD never became ready; check: kubectl -n $NS logs $POD"
  fi
fi

cleanup() {
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null || true
  if [ "$CREATED" = 1 ] && [ "$KEEP" = 0 ]; then
    kubectl -n "$NS" delete pod "$POD" --ignore-not-found --now >/dev/null 2>&1 || true
    log "deleted $POD"
  elif [ "$CREATED" = 1 ]; then
    log "left $POD running (--keep); delete with: kubectl -n $NS delete pod $POD"
  fi
}
trap cleanup EXIT

kubectl -n "$NS" port-forward "pod/$POD" "$DATA_PORT:8080" "$CTRL_PORT:8081" >/dev/null 2>&1 &
PF_PID=$!
# Wait for the tunnel rather than sleeping a fixed amount: it is usually ready
# in well under a second, and occasionally not.
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:${CTRL_PORT}/internal/v1/probe" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -sf "http://127.0.0.1:${CTRL_PORT}/internal/v1/probe" >/dev/null 2>&1 \
  || die "port-forward to $POD did not come up"

TIER=$(curl -s "http://127.0.0.1:${CTRL_PORT}/internal/v1/probe" \
       | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["isolationTier"], "-", d["reason"])')
log "tier   : $TIER"

# Bind is what turns a warm Pod into a specific sandbox: it is the only moment
# the filesystem spec exists, and therefore the only moment bubblewrap can be
# built (DESIGN.md §04.1).
# An existing warm Pod carries a token the controller minted; read it back
# rather than guessing.
if [ -z "${CREATED:-0}" ] || [ "$CREATED" = 0 ]; then
  FOUND=$(kubectl -n "$NS" get pod "$POD" \
    -o jsonpath='{.spec.containers[0].env[?(@.name=="JDIX_CONTROL_TOKEN")].value}' 2>/dev/null || true)
  [ -n "$FOUND" ] && CTL_TOKEN=$FOUND
fi

BIND=$(curl -s -X POST "http://127.0.0.1:${CTRL_PORT}/internal/v1/bind" \
  -H "Authorization: Bearer ${CTL_TOKEN}" \
  -H 'Content-Type: application/json' -d @- <<JSON
{
  "sandboxId": "sbx-dev-$$",
  "tenant": "dev",
  "token": "${SBX_TOKEN}",
  "ttlSeconds": 1800,
  "env": {"RUN_ID": "dev"},
  "secrets": {"DEV_SECRET": "s3cr3t"},
  "filesystem": { "workspace": { "path": "/workspace" } }
}
JSON
)
echo "$BIND" | grep -q '"sandboxId"' || die "bind failed: $BIND"
log "bound  : $(echo "$BIND" | python3 -c 'import sys,json; print(json.load(sys.stdin)["sandboxId"])')"

if [ "$SHOW_ARGS" = 1 ]; then
  echo "$BIND" | python3 -c '
import sys, json
args = json.load(sys.stdin).get("bwrapArgs", [])
line = []
for a in args:
    line.append(a)
    if a.startswith("--") and len(line) > 1:
        print(" ", " ".join(line[:-1])); line = [a]
print(" ", " ".join(line))'
  exit 0
fi

run() { # run <command>
  curl -s -X POST "http://127.0.0.1:${DATA_PORT}/v1/exec" \
    -H "Authorization: Bearer ${SBX_TOKEN}" -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"cmd": sys.argv[1], "timeoutSeconds": 60}))' "$1")"
}

if [ -n "$CMD" ]; then
  run "$CMD" | python3 -c '
import sys, json
r = json.load(sys.stdin)
sys.stdout.write(r.get("stdout", ""))
sys.stderr.write(r.get("stderr", ""))
sys.exit(r.get("exitCode", 0))'
  exit $?
fi

cat >&2 <<EOF

  Sandbox shell. Commands run inside the namespace, one exec per line.
  Try:  id · pwd · ls /opt/jdix · cat /proc/1/cmdline · echo \$DEV_SECRET
  Ctrl-D to finish.

EOF
while IFS= read -r -p "sandbox> " line; do
  [ -z "$line" ] && continue
  run "$line" | python3 -c '
import sys, json
r = json.load(sys.stdin)
sys.stdout.write(r.get("stdout", ""))
sys.stderr.write(r.get("stderr", ""))
code = r.get("exitCode", 0)
if code:
    sys.stderr.write("[exit {}]\n".format(code))'
done
