#!/usr/bin/env bash
# End-to-end demo of the jdix-sandbox data plane on a Linux host.
#
# Starts jdix-execd, binds a sandbox to it exactly as jdix-controller would,
# then drives the data plane. Requires bubblewrap; run hack/probe-isolation.sh
# first if you are unsure the host supports it.
set -euo pipefail

BIN=${BIN:-./bin}
BASE=$(mktemp -d /tmp/jdix-demo.XXXX)
TOKEN=internal-demo-token
SBT=sbt-demo-token
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$BASE"' EXIT

if [ ! -x "$BIN/jdix-execd" ]; then echo "build first: make build" >&2; exit 1; fi
command -v bwrap >/dev/null || { echo "bubblewrap is not installed" >&2; exit 1; }

echo "$TOKEN" > "$BASE/token"
mkdir -p "$BASE/vol/corpus"
echo "hello from the corpus volume" > "$BASE/vol/corpus/readme.txt"

echo "== starting jdix-execd =="
sudo "$BIN/jdix-execd" \
  --data-addr 127.0.0.1:18080 \
  --control-addr 127.0.0.1:18081 \
  --bwrap "$(command -v bwrap)" \
  --volumes "corpus=/var/lib/jdix/vol/corpus" \
  --internal-token-file "$BASE/token" &
sleep 1

ctl() { curl -sS -H "Authorization: Bearer $TOKEN" "$@"; }
data() { curl -sS -H "Authorization: Bearer $SBT" "$@"; }

echo "== measured isolation tier =="
ctl http://127.0.0.1:18081/internal/v1/probe; echo

echo "== binding a sandbox =="
ctl -X POST http://127.0.0.1:18081/internal/v1/bind \
  -H 'Content-Type: application/json' -d @- <<JSON | head -c 400; echo
{
  "sandboxId": "sbx_demo",
  "tenant": "demo",
  "token": "$SBT",
  "ttlSeconds": 600,
  "env": {"RUN_ID": "42"},
  "secrets": {"DEMO_SECRET": "s3cr3t"},
  "filesystem": {
    "workspace": {"path": "/workspace"},
    "mounts": [
      {"path": "/data/corpus", "source": {"volume": "corpus"}, "readOnly": true}
    ]
  }
}
JSON

echo "== the sandbox sees its workspace and the mounted volume =="
data -X POST http://127.0.0.1:18080/v1/exec -H 'Content-Type: application/json' \
  -d '{"cmd":"id; pwd; ls /data/corpus; cat /data/corpus/readme.txt; echo $RUN_ID-$DEMO_SECRET"}'; echo

echo "== and cannot see the platform's own files =="
data -X POST http://127.0.0.1:18080/v1/exec -H 'Content-Type: application/json' \
  -d '{"cmd":"ls /opt/jdix 2>&1; ls /var/run/secrets 2>&1; cat /proc/1/environ 2>&1 | head -c 60"}'; echo

echo "== the file API refuses to leave the workspace =="
data "http://127.0.0.1:18080/v1/files?path=/etc/passwd"; echo

echo "== an unauthenticated caller gets nothing =="
curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18080/v1/exec \
  -H 'Content-Type: application/json' -d '{"cmd":"id"}'

echo "== done =="
