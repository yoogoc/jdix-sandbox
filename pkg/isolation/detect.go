// Package isolation measures what a node can actually enforce.
//
// It probes by running bubblewrap for real rather than by reading sysctls: the
// question that matters is "will a sandbox start on this node", and only an
// actual attempt answers it. The result gates whether a Pod may join the warm
// pool at all (DESIGN.md §04.5).
package isolation

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	"jdix.io/sandbox/pkg/bwrap"
)

// Result is what execd reports on /internal/v1/status and writes to the Pod
// annotation the pool controller reads.
type Result struct {
	Tier   bwrap.Tier        `json:"tier"`
	Reason string            `json:"reason"`
	Probes map[string]string `json:"probes"`
}

// probeTimeout bounds a single bwrap invocation. A node so loaded that bwrap
// cannot start in this window should not be handing out sandboxes anyway.
const probeTimeout = 5 * time.Second

// Detect measures the strongest tier this node supports.
//
// It never returns an error: an undetectable node is a chroot-tier node, and
// the caller decides whether that clears the template's minIsolationTier.
func Detect(ctx context.Context, bwrapPath string) Result {
	res := Result{Probes: map[string]string{}}
	res.Probes["goos"] = runtime.GOOS

	if runtime.GOOS != "linux" {
		res.Tier = bwrap.TierChroot
		res.Reason = "not running on Linux; namespaces unavailable"
		return res
	}
	if bwrapPath == "" {
		bwrapPath = "bwrap"
	}
	if _, err := exec.LookPath(bwrapPath); err != nil {
		res.Tier = bwrap.TierChroot
		res.Reason = "bubblewrap not found: " + err.Error()
		res.Probes["bwrap"] = "missing"
		return res
	}

	if out, err := runProbe(ctx, bwrapPath, probeArgsUserns()); err == nil {
		res.Tier = bwrap.TierUserns
		res.Reason = "unprivileged user namespace available"
		res.Probes["userns"] = "ok"
		return res
	} else {
		res.Probes["userns"] = trim(out, err)
	}

	if out, err := runProbe(ctx, bwrapPath, probeArgsCapAdmin()); err == nil {
		res.Tier = bwrap.TierCapAdmin
		res.Reason = "mount namespace available via CAP_SYS_ADMIN; no user namespace"
		res.Probes["capadmin"] = "ok"
		return res
	} else {
		res.Probes["capadmin"] = trim(out, err)
	}

	res.Tier = bwrap.TierChroot
	if hasChroot() {
		res.Reason = "no mount namespace; CAP_SYS_CHROOT present"
		res.Probes["chroot"] = "ok"
	} else {
		res.Reason = "no mount namespace and no CAP_SYS_CHROOT; isolation is limited to uid separation"
		res.Probes["chroot"] = "missing"
	}
	return res
}

// probeBase is the smallest mount set that still proves the namespace works.
func probeBase() []string {
	return []string{
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/sbin", "/sbin",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--die-with-parent",
		"--", "/bin/true",
	}
}

func probeArgsUserns() []string {
	a := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--uid", "1000", "--gid", "1000"}
	return append(a, probeBase()...)
}

func probeArgsCapAdmin() []string {
	a := []string{"--unshare-pid", "--unshare-ipc", "--unshare-uts"}
	return append(a, probeBase()...)
}

func runProbe(ctx context.Context, bin string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

func trim(out []byte, err error) string {
	s := string(out)
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return err.Error()
	}
	return s
}
