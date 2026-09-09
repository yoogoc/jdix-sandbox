#!/usr/bin/env bash
# Stand up a debuggable jdix-sandbox environment on a local cluster.
#
# Creates the namespace, the control-plane token, an approved SandboxTemplate
# and a SandboxPool, then leaves the cluster ready for a controller — whether
# that controller runs here (hack/dev/run.sh) or in-cluster.
#
#   hack/dev/up.sh            # namespace jdix-dev, image jdix/sandbox-base:dev
#   NS=scratch hack/dev/up.sh
set -euo pipefail

NS=${NS:-jdix-dev}
IMAGE=${IMAGE:-jdix/sandbox-base:dev}
TEMPLATE=${TEMPLATE:-py312-local}
POOL_REPLICAS=${POOL_REPLICAS:-2}

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

CTX=$(kubectl config current-context)
SERVER=$(kubectl config view -o jsonpath="{.clusters[?(@.name=='$CTX')].cluster.server}")
case "$SERVER" in
  *127.0.0.1*|*localhost*|*192.168.*|*::1*) ;;
  *) die "context '$CTX' is not local; refusing to create objects on it" ;;
esac
log "context: $CTX"

# ── the image ───────────────────────────────────────────────────────────────
# A locally built image was never pushed anywhere, so no registry can resolve
# it. The template says so explicitly rather than working around it: resolve
# false stops the controller asking a registry, and pullPolicy Never tells the
# kubelet the image is already on the node — which it is, because
# hack/load-image.sh put it there.
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
log "image  : $IMAGE (unresolved, node-local)"

# ── template and pool ───────────────────────────────────────────────────────
# The approved-by annotation only matters for templates with volumes; it is set
# here so adding a volume later does not silently park the template in review.
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: sandbox.jdix.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: ${TEMPLATE}
  annotations:
    sandbox.jdix.io/approved-by: local-dev
spec:
  minIsolationTier: userns
  defaultTTLSeconds: 600
  maxTTLSeconds: 3600
  image:
    ref: ${IMAGE}
    resolve: false
    pullPolicy: Never
  filesystemDefaults:
    workspace: { sizeLimit: 1Gi }
    allowSystemPaths: [/usr, /bin, /sbin, /lib, /lib64, /etc/ssl]
  network:
    egress: restricted
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits:   { cpu: "1",  memory: 1Gi }
---
apiVersion: sandbox.jdix.io/v1alpha1
kind: SandboxPool
metadata: { name: ${TEMPLATE} }
spec:
  templateRef: ${TEMPLATE}
  replicas: ${POOL_REPLICAS}
  minReplicas: 0
  maxReplicas: 5
  notReadyTimeoutSeconds: 180
  idleTTLSeconds: 3600
  maxSurge: 2
YAML
log "template: ${NS}/${TEMPLATE}  pool replicas=${POOL_REPLICAS}"

cat >&2 <<EOF

  Ready. Nothing supplies the pool until a controller runs:

    hack/dev/run.sh controller          # on this host
    hack/dev/run.sh apiserver           # in another terminal
    hack/dev/sandbox.sh --pool ${TEMPLATE}   # or drive a Pod directly, no controller

  Watch it: kubectl -n ${NS} get sbxpool,sbx,pods -w
  Tear down: hack/dev/down.sh
EOF
