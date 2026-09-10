#!/usr/bin/env bash
# Run one control-plane component on this host against the local cluster.
#
# Running them here rather than in the cluster is the whole point: you get a
# normal debugger, normal logs, and a rebuild loop measured in seconds.
#
#   hack/dev/run.sh controller
#   hack/dev/run.sh server               # jdix-server --role=all: both ports
#   hack/dev/run.sh apiserver            # just the control plane
#   hack/dev/run.sh gateway              # just the data plane
#   DLV=1 hack/dev/run.sh controller     # wait for a debugger on :2345
#   TRANSPORT=direct hack/dev/run.sh ...  # dial Pod IPs instead of proxying
#
# `server` is one process and one terminal, which is what you want most of the
# time. The split forms exist so you can restart one half without dropping the
# other's connections while you are working on it.
#
# Both the controller and the gateway default to reaching Pods through the API
# server's pod proxy here, so they work from a laptop with no route to the Pod
# network. Set TRANSPORT=direct if hack/dev/preflight.sh said the Pod network is
# routable; it is faster, and it is what production uses.
set -euo pipefail

COMPONENT=${1:-}
NS=${NS:-jdix-dev}
IMAGE=${IMAGE:-jdix/sandbox-base:dev}
DLV=${DLV:-0}
DLV_PORT=${DLV_PORT:-2345}

usage() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 1; }
[ -n "$COMPONENT" ] || usage

# The controller needs the sandbox image by digest, the same way the template
# references it, or the Pods it creates will not match what is on the node.
platform_image() {
  kubectl -n "$NS" get sbxtpl -o jsonpath='{.items[0].spec.image.ref}' 2>/dev/null || echo "$IMAGE"
}

launch() { # launch <pkg> <args...>
  local pkg=$1; shift
  if [ "$DLV" = "1" ]; then
    command -v dlv >/dev/null || { echo "dlv not installed: go install github.com/go-delve/delve/cmd/dlv@latest" >&2; exit 1; }
    echo "  delve listening on :$DLV_PORT — attach, then continue" >&2
    exec dlv debug "$pkg" --headless --listen=":$DLV_PORT" --api-version=2 --accept-multiclient -- "$@"
  fi
  exec go run "$pkg" "$@"
}

case "$COMPONENT" in
  controller)
    echo "  controller → namespace $NS, platform image $(platform_image)" >&2
    # Binding is an HTTP call to podIP:8081, and sandbox Pods have no Service.
    # Off-cluster that route usually does not exist, so the default here goes
    # through the API server's pod proxy instead — the same kubeconfig that
    # reaches the cluster reaches every Pod. Set TRANSPORT=direct if
    # hack/dev/preflight.sh said the Pod network is routable; it is faster and
    # is what production uses.
    launch ./cmd/jdix-controller \
      --platform-image="$(platform_image)" \
      --execd-transport="${TRANSPORT:-apiserver-proxy}" \
      --endpoint-mode=path \
      --endpoint-base="${ENDPOINT_BASE:-http://127.0.0.1:8090}" \
      --leader-elect=false \
      --metrics-bind-address=:18080 \
      --health-probe-bind-address=:18081 \
      --zap-devel=true
    ;;

  server|apiserver|gateway)
    # No --database-url: the in-memory store plus --dev-seed prints a usable API
    # key on start-up, so there is nothing to provision before the first call.
    #
    # Prefer `server` when you want to make calls. The data plane authenticates
    # the same API key as the control plane, so running `apiserver` and
    # `gateway` as two processes gives them two in-memory stores and two
    # different dev keys — the one printed by `apiserver` will not open a
    # sandbox through `gateway`. Split them only with a real --database-url.
    # Path routing needs no DNS locally either — the sandbox id is in the URL,
    # so http://127.0.0.1:8090/s/<id>/v1/exec works straight from curl.
    case "$COMPONENT" in
      server)    ROLE=all;     BANNER="control plane :8000 + data plane :8090" ;;
      apiserver) ROLE=server;  BANNER="control plane → http://127.0.0.1:8000" ;;
      gateway)   ROLE=gateway; BANNER="data plane → http://127.0.0.1:8090, sandboxes at /s/<id>/" ;;
    esac
    echo "  jdix-server --role=$ROLE → $BANNER" >&2
    [ "$ROLE" = gateway ] || echo "  (in-memory store; the dev API key is printed below)" >&2
    launch ./cmd/jdix-server \
      --role="$ROLE" \
      --sandbox-transport="${TRANSPORT:-apiserver-proxy}" \
      --addr=:8000 \
      --data-addr=:8090 \
      --route-mode=path \
      --dev-seed \
      --ready-timeout=15s \
      --log-level=debug
    ;;

  *)
    echo "unknown component: $COMPONENT" >&2
    usage
    ;;
esac
