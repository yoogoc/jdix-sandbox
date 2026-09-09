#!/usr/bin/env bash
# Load a locally built image into a k3s-style cluster's containerd.
#
# Neither k3s nor OrbStack's Kubernetes shares the Docker image store, so an
# image built with `docker build` is invisible to the kubelet — a Pod referring
# to it fails with ErrImageNeverPull, or worse, tries to pull it from Docker Hub
# and reports a confusing authorization error.
#
# The image is streamed straight from `docker save` into `ctr images import` on
# the node, so nothing large is ever written to disk on either side.
#
# Usage:
#   hack/load-image.sh [image]          # default: jdix/sandbox-base:dev
#   NAMESPACE=kube-system hack/load-image.sh
set -euo pipefail

IMAGE=${1:-${IMAGE:-jdix/sandbox-base:dev}}
NAMESPACE=${NAMESPACE:-default}
LOADER=${LOADER:-jdix-image-loader}
# Already present on most k3s nodes, so importing does not itself need a pull.
HELPER_IMAGE=${HELPER_IMAGE:-docker.io/library/alpine:3.20}

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v docker  >/dev/null || die "docker not found"
command -v kubectl >/dev/null || die "kubectl not found"
docker image inspect "$IMAGE" >/dev/null 2>&1 || die "no such local image: $IMAGE (run 'make image' first)"

CONTEXT=$(kubectl config current-context)
log "cluster : $CONTEXT"
log "image   : $IMAGE ($(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.0f MiB", $1/1048576}'), $(docker image inspect "$IMAGE" --format '{{.Os}}/{{.Architecture}}'))"

# A remote cluster cannot be reached this way, and quietly importing into the
# wrong place would be worse than refusing.
case "$(kubectl config view -o jsonpath="{.clusters[?(@.name=='$CONTEXT')].cluster.server}")" in
  *127.0.0.1*|*localhost*|*192.168.*|*::1*) ;;
  *) die "context '$CONTEXT' does not look local; push to a registry instead of importing" ;;
esac

cleanup() { kubectl -n "$NAMESPACE" delete pod "$LOADER" --ignore-not-found --now >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

log "starting loader pod..."
kubectl -n "$NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${LOADER}
spec:
  restartPolicy: Never
  containers:
    - name: loader
      image: ${HELPER_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sleep", "600"]
      securityContext:
        privileged: true
      volumeMounts:
        - { name: host, mountPath: /host }
  volumes:
    - name: host
      hostPath: { path: / }
YAML
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$LOADER" --timeout=120s >/dev/null

# k3s ships ctr as a symlink to the k3s binary; a stock containerd node has its
# own. Find whichever exists rather than guessing.
CTR=$(kubectl -n "$NAMESPACE" exec "$LOADER" -- sh -c '
  for p in /host/usr/local/bin/ctr /host/usr/bin/ctr /host/bin/ctr; do
    [ -x "$p" ] && { echo "$p"; exit 0; }
  done' 2>/dev/null || true)
[ -n "$CTR" ] || die "no ctr binary on the node; this script assumes a containerd runtime"

SOCK=$(kubectl -n "$NAMESPACE" exec "$LOADER" -- sh -c '
  for s in /host/run/k3s/containerd/containerd.sock /host/run/containerd/containerd.sock; do
    [ -S "$s" ] && { echo "$s"; exit 0; }
  done' 2>/dev/null || true)
[ -n "$SOCK" ] || die "no containerd socket on the node"

log "importing via $CTR"
# Namespace k8s.io is where the CRI looks; importing anywhere else is invisible
# to the kubelet.
docker save "$IMAGE" \
  | kubectl -n "$NAMESPACE" exec -i "$LOADER" -- "$CTR" --address "$SOCK" -n k8s.io images import - \
  | sed 's/^/  /' >&2

kubectl -n "$NAMESPACE" exec "$LOADER" -- "$CTR" --address "$SOCK" -n k8s.io images ls \
  | awk -v img="${IMAGE##*/}" 'NR==1 || index($1, img)' | cut -c1-120 | sed 's/^/  /' >&2

log "done — reference it with imagePullPolicy: Never"
