# jdix-sandbox

Isolated code-execution sandboxes for AI agents, on Kubernetes.

One sandbox owns one single-use Pod. A warm pool absorbs the start-up cost so
`create → ready` is a couple of hundred milliseconds. Inside the Pod,
bubblewrap builds a second boundary from the sandbox's own spec, so tenant code
cannot reach the platform's credentials, its binaries, or anything the spec did
not ask for.

The full design, including the decisions that were rejected and why, is in
[docs/DESIGN.md](docs/DESIGN.md).

## Where the work is

| Milestone | Scope | State |
|---|---|---|
| M0 | Node isolation probe | ✅ `hack/probe-isolation.sh` + runtime detection in `pkg/isolation` |
| M1 | Data plane: `jdix-execd`, `jdix-init`, bwrap argv generator | ✅ implemented, tested, race-clean |
| M2 | Control plane: `Sandbox` CRD, controller, apiserver, Postgres | ✅ CRDs, three reconcilers, REST API, API keys, quota, idempotency |
| M3 | Warm pool: `SandboxTemplate` / `SandboxPool`, bind CAS | ✅ supply, tier gating, rollout, orphan collection |
| M4 | Gateway and port exposure | ✅ subdomain routing, token gate, NetworkPolicy manifests |
| M5 | CSI volumes, image admission, warm-up budget | ✅ volumes, admission, warm-up request queue and budget enforcement |
| M6 | Python and Go SDKs | ✅ both, with tests |
| M7 | Console + OIDC | ⬜ not started |
| M8 | Hardening, load and chaos testing | ⬜ not started |

## Layout

```
cmd/jdix-apiserver  tenant-facing REST API: keys, quota, admission
cmd/jdix-controller reconciles Sandbox, SandboxTemplate, SandboxPool
cmd/jdix-gateway    publishes sandboxes, by path or by subdomain (DESIGN.md §8)
cmd/jdix-execd      container PID 1, outside the namespace: measures the node,
                    binds a sandbox, proxies the data plane
cmd/jdix-init       PID 1 inside the namespace: reaps orphans, runs commands,
                    serves the file API

pkg/bwrap           turns an untrusted filesystem spec into a bubblewrap argv
pkg/safepath        openat2(RESOLVE_BENEATH) with a portable fallback
pkg/isolation       measures which isolation tier a node can actually enforce
pkg/execd           control plane, auth, TTL, reverse proxy
pkg/initd           exec, PTY, files — everything that runs inside
pkg/apis/...        the three custom resources
pkg/controller      binding, warm-pool supply, template admission
pkg/apiserver       API keys, quota, idempotency, the REST surface
pkg/gateway         hostname routing and the public reverse proxy

sdk/go              Go client (its own module, so users do not inherit k8s deps)
sdk/python          Python client
config/             CRDs, RBAC, manager, NetworkPolicy, schema.sql, samples
hack/               node probe, end-to-end demo
```

## Two Store implementations, one set of semantics

`pkg/apiserver` runs against Postgres in production and an in-memory store in
development. They drifted once already — one derived the allocated warm-pool
budget from the approved requests while the other kept a counter, so the same
calls gave different answers. `TestStoreConformance` now states the semantics
both must satisfy and runs against whichever backends are available:

```sh
go test ./pkg/apiserver                     # in-memory only
DATABASE_URL=postgres://… go test ./pkg/apiserver   # both
```

## The two places that get fuzzed

Tenant-controlled paths reach a privileged operation in exactly two places, and
both have dedicated fuzz targets:

- `pkg/bwrap` — the argv generator. A spec that could mount over `/proc`,
  `/opt/jdix` or the IPC socket would undo the whole boundary.
- `pkg/safepath` — path resolution. Lexical checks are not enough; a symlink
  planted inside a volume can point anywhere.

```sh
make fuzz              # 60s each
FUZZTIME=10m make fuzz
```

## Running it

Tests and the argv generator work anywhere. Actually starting a sandbox needs
Linux with bubblewrap.

```sh
make test          # Go and Python, unit and integration
make test-race
make build         # host binaries into ./bin
make generate      # regenerate deepcopy, CRDs and RBAC from markers

# On a Linux host, check what the kernel will allow:
./hack/probe-isolation.sh          # exit 0=userns 1=capadmin 2=chroot 3=none

# Then drive the whole data plane end to end:
make demo
```

`make demo` starts `jdix-execd`, binds a sandbox the way the controller will,
and shows the boundary working: the sandbox sees its workspace and its mounted
volume, and cannot see `/opt/jdix`, the ServiceAccount token, or `/proc/1`.

## Isolation tiers

bubblewrap needs either an unprivileged user namespace or `CAP_SYS_ADMIN`.
Neither is guaranteed, so a node is measured rather than assumed, and the
measurement gates whether the Pod may serve a given template.

| Tier | Requires | Notes |
|---|---|---|
| `userns` | unprivileged user namespaces | target state; no extra capability needed |
| `capadmin` | `CAP_SYS_ADMIN` | works, but widens the escape surface |
| `chroot` | `CAP_SYS_CHROOT` | no mount namespace; `spec.filesystem.mounts` is rejected |

A node that measures below a template's `minIsolationTier` never joins that
template's warm pool, so the failure lands on the supply path instead of a
user's request.

## Known limits

- Isolation is namespace-level, not VM-level. With tenant-supplied images on
  shared nodes and no gVisor, a kernel privilege-escalation bug crosses the
  tenant boundary. This is a deliberate, recorded trade-off — see DESIGN.md
  §03.5 before promising anything stronger to a customer.
- The non-Linux `safepath` fallback is for development only. It refuses
  symlinks but is not atomic against a concurrent rename.
