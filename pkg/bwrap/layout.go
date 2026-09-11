// Package bwrap turns an untrusted api.FilesystemSpec into a bubblewrap argv.
//
// This is one of the two places in the platform where tenant-controlled paths
// reach a privileged operation (the other is the file API). Everything here is
// written on the assumption that the spec is hostile: see Validate.
package bwrap

import "path"

// Tier is the isolation level the node was measured to support (DESIGN.md §03.2).
type Tier string

const (
	// TierFilesystem is a distinct capability, not a rank in the legacy hierarchy.
	TierFilesystem Tier = "filesystem"
	TierUserns     Tier = "userns"   // unprivileged user namespace — target state
	TierCapAdmin   Tier = "capadmin" // CAP_SYS_ADMIN, no user namespace
	TierChroot     Tier = "chroot"   // no mount namespace at all
)

// Rank orders tiers so a template can demand a minimum.
func (t Tier) Rank() int {
	switch t {
	case TierUserns:
		return 3
	case TierCapAdmin:
		return 2
	case TierChroot:
		return 1
	}
	return 0
}

// AtLeast reports whether t satisfies a template's minIsolationTier.
func (t Tier) AtLeast(min Tier) bool {
	if t == TierFilesystem || min == TierFilesystem {
		return t == min
	}
	return t.Rank() > 0 && min.Rank() > 0 && t.Rank() >= min.Rank()
}

// Layout is where the platform put its own files inside the Pod. None of it is
// tenant-controlled; it comes from the initContainer and the Pod spec.
// Two roots, with different owners and different lifetimes:
//
//	/opt/jdix      installed by the initContainer, read-only at runtime
//	/var/lib/jdix  written by execd as it runs
//
// The skeleton /etc files belong to the second: execd generates them at
// start-up, so they are state, not installation. Putting them under /opt/jdix
// would mean execd needs write access to the directory holding the platform's
// own binaries, which is exactly what should not be writable.
type Layout struct {
	BinDir       string // /opt/jdix/bin       — execd, jdix-init, bwrap
	SkelDir      string // /var/lib/jdix/skel  — passwd, group, resolv.conf
	WorkspaceDir string // /var/lib/jdix/workspace
	VolumeRoot   string // /var/lib/jdix/vol   — CSI/NFS volumes mount here
	IPCDir       string // /var/lib/jdix/ipc   — unix socket dir on the outside
	IPCMount     string // /run/jdix           — where IPCDir appears inside
	InitTarget   string // /jdix-init          — jdix-init's path inside
}

// DefaultLayout matches the paths baked into the initContainer and the Helm chart.
func DefaultLayout() Layout {
	return Layout{
		BinDir:       "/opt/jdix/bin",
		SkelDir:      "/var/lib/jdix/skel",
		WorkspaceDir: "/var/lib/jdix/workspace",
		VolumeRoot:   "/var/lib/jdix/vol",
		IPCDir:       "/var/lib/jdix/ipc",
		IPCMount:     "/run/jdix",
		InitTarget:   "/jdix-init",
	}
}

// PlatformRoot is the prefix the sandbox must never be able to see.
const PlatformRoot = "/opt/jdix"

// StateRoot holds the workspace, volumes and IPC socket on the outside.
const StateRoot = "/var/lib/jdix"

// DefaultSystemPaths is the platform allowlist for spec.allowSystemPaths.
// A tenant may select a subset; it may never add to this list.
func DefaultSystemPaths() []string {
	return []string{
		"/usr", "/bin", "/sbin", "/lib", "/lib64",
		"/etc/ssl", "/etc/ca-certificates", "/etc/alternatives",
	}
}

// MaxMounts caps spec.mounts so a pathological list cannot stall bwrap startup
// (DESIGN.md §03.4).
const MaxMounts = 32

// Policy is the platform-side half of the input: everything the tenant does not
// get to choose.
type Policy struct {
	// SourceFDs pins tenant mount targets to inherited bwrap file descriptors.
	SourceFDs map[string]int

	Tier            Tier
	UID, GID        int
	Hostname        string
	MaxMounts       int
	SystemAllowlist []string
	// Volumes maps a template-declared volume name to its mount path inside the
	// Pod. A spec.mounts entry naming a volume absent from this map is rejected;
	// this is what stops a tenant from binding an arbitrary host path.
	Volumes map[string]string
	// IsDirFunc decides whether a spec.hide target is a directory. Left nil it
	// stats the real filesystem; tests inject a stub.
	IsDirFunc func(path string) bool
}

// DefaultPolicy returns a policy for the given tier with the platform defaults.
func DefaultPolicy(tier Tier) Policy {
	return Policy{
		Tier:            tier,
		UID:             1000,
		GID:             1000,
		Hostname:        "sandbox",
		MaxMounts:       MaxMounts,
		SystemAllowlist: DefaultSystemPaths(),
		Volumes:         map[string]string{},
	}
}

// clean normalises a path for comparison. It does not touch the filesystem.
func clean(p string) string { return path.Clean(p) }
