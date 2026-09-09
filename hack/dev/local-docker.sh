#!/usr/bin/env bash
# Debug execd and jdix-init with no Kubernetes at all.
#
# The fastest possible loop for pkg/bwrap, pkg/initd and pkg/execd: rebuild,
# rerun, done. What you give up is the isolation tier — a plain container has no
# way to grant unprivileged user namespaces, so this measures `chroot` and
# spec.filesystem.mounts is rejected. Use hack/dev/sandbox.sh when the tier
# itself is what you are debugging.
#
#   hack/dev/local-docker.sh                    # bind and open a shell
#   hack/dev/local-docker.sh -- 'id; pwd'       # run one command
#   PRIVILEGED=1 hack/dev/local-docker.sh       # --privileged, reaches the userns tier
set -euo pipefail

IMAGE=${IMAGE:-jdix/sandbox-base:dev}
NAME=${NAME:-jdix-local-$$}
TOKEN=${SBX_TOKEN:-sbt-local-$$}
# execd refuses to serve an unauthenticated control plane, so give it one.
CTL_TOKEN=${CTL_TOKEN:-jct-local-$$}
DATA=${DATA_PORT:-18080}
CTRL=${CTRL_PORT:-18081}
PRIVILEGED=${PRIVILEGED:-0}
CMD=""
[ "${1:-}" = "--" ] && { shift; CMD="$*"; }

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

docker image inspect "$IMAGE" >/dev/null 2>&1 || die "no such image: $IMAGE (run 'make image')"

EXTRA=""
if [ "$PRIVILEGED" = "1" ]; then
  # --privileged also unmasks /proc and drops the seccomp profile, which are the
  # same two things Kubernetes needs hostUsers+procMount+Unconfined for.
  EXTRA="--privileged"
  log "privileged: expect the userns tier"
fi

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

# shellcheck disable=SC2086
docker run -d --name "$NAME" $EXTRA \
  -e "JDIX_CONTROL_TOKEN=${CTL_TOKEN}" \
  -p "127.0.0.1:${DATA}:8080" -p "127.0.0.1:${CTRL}:8081" \
  "$IMAGE" --log-level=debug >/dev/null
log "container: $NAME"

for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:${CTRL}/internal/v1/probe" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -sf "http://127.0.0.1:${CTRL}/internal/v1/probe" >/dev/null 2>&1 || {
  docker logs "$NAME" >&2; die "execd did not come up"
}
log "tier   : $(curl -s "http://127.0.0.1:${CTRL}/internal/v1/probe" \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["isolationTier"], "-", d["reason"])')"

curl -s -X POST "http://127.0.0.1:${CTRL}/internal/v1/bind" \
  -H "Authorization: Bearer ${CTL_TOKEN}" \
  -H 'Content-Type: application/json' -d @- >/dev/null <<JSON
{ "sandboxId": "sbx-local", "tenant": "dev", "token": "${TOKEN}", "ttlSeconds": 1800,
  "env": {"RUN_ID": "local"}, "secrets": {"DEV_SECRET": "s3cr3t"},
  "filesystem": { "workspace": { "path": "/workspace" } } }
JSON
log "bound  : sbx-local"

run() {
  curl -s -X POST "http://127.0.0.1:${DATA}/v1/exec" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"cmd": sys.argv[1], "timeoutSeconds": 60}))' "$1")" \
  | python3 -c '
import sys, json
r = json.load(sys.stdin)
sys.stdout.write(r.get("stdout", "")); sys.stderr.write(r.get("stderr", ""))
sys.exit(r.get("exitCode", 0) if r.get("exitCode", 0) < 126 else 0)'
}

if [ -n "$CMD" ]; then run "$CMD"; exit $?; fi

echo >&2
echo "  Sandbox shell (docker). Ctrl-D to finish; container is removed on exit." >&2
echo >&2
while IFS= read -r -p "sandbox> " line; do
  [ -z "$line" ] && continue
  run "$line" || true
done
