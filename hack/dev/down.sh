#!/usr/bin/env bash
# Remove everything hack/dev/up.sh created.
#
# Leaves the CRDs and the imported image alone: those are installed once and
# reused, and re-importing 170MB on every iteration is a waste. Pass --all to
# remove them too.
set -euo pipefail

NS=${NS:-jdix-dev}
ALL=${1:-}

kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
echo "  namespace $NS deleting (async)" >&2

# A Sandbox carries a finalizer so its Pod cannot be orphaned. If no controller
# is running to clear it, deleting the namespace stalls — so say so rather than
# letting it hang silently.
if kubectl get sbx -n "$NS" >/dev/null 2>&1; then
  remaining=$(kubectl get sbx -n "$NS" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [ "$remaining" != "0" ]; then
    cat >&2 <<EOF
  ! $remaining Sandbox object(s) still present. Their finalizer is cleared by
    jdix-controller; with no controller running the namespace will not finish
    deleting. Either start one, or force it:
      kubectl -n $NS patch sbx <name> -p '{"metadata":{"finalizers":[]}}' --type=merge
EOF
  fi
fi

if [ "$ALL" = "--all" ]; then
  kubectl delete -f config/crd --ignore-not-found >/dev/null 2>&1 || true
  echo "  CRDs removed" >&2
  echo "  (the imported node image is left in place; it is harmless and slow to rebuild)" >&2
fi
