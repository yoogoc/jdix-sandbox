package bwrap

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"jdix.io/sandbox/pkg/api"
)

// InitSocketName is the unix socket jdix-init listens on, inside the namespace.
const InitSocketName = "init.sock"

// Generate turns a validated spec into a complete bubblewrap argv.
//
// Two things about the output are load-bearing:
//
//   - --clearenv comes before any --setenv, so the sandbox never inherits
//     execd's environment (which holds the control-plane token).
//   - tenant env and secrets are NOT passed here. They travel over the unix
//     socket to jdix-init, which applies them when spawning user processes, so
//     they never appear in a command line.
func Generate(spec api.FilesystemSpec, p Policy, l Layout) ([]string, error) {
	if err := p.Validate(spec, l); err != nil {
		return nil, err
	}

	var a []string
	add := func(xs ...string) { a = append(a, xs...) }

	// ── namespaces ────────────────────────────────────────────────────────
	if p.Tier == TierUserns || p.Tier == TierFilesystem {
		add("--unshare-user")
	}
	if p.Tier != TierFilesystem {
		add("--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try")
	}

	// Never --unshare-net: the sandbox needs egress. Network isolation is a
	// NetworkPolicy concern at the Pod level (DESIGN.md §08).

	if p.Tier == TierUserns || p.Tier == TierFilesystem {
		// bwrap can only map ids when it owns a user namespace. Under
		// TierCapAdmin jdix-init drops privileges itself instead.
		add("--uid", itoa(p.UID), "--gid", itoa(p.GID))
	}
	if p.Hostname != "" && p.Tier != TierFilesystem {
		add("--hostname", p.Hostname)
	}

	// ── system skeleton (template-determined, sandbox-independent) ────────
	sys := spec.AllowSystemPaths
	if len(sys) == 0 {
		sys = p.SystemAllowlist
	}
	for _, s := range sys {
		// -try: /lib64 and /sbin are absent on some images and architectures.
		add("--ro-bind-try", s, s)
	}
	add("--ro-bind", path.Join(l.SkelDir, "passwd"), "/etc/passwd")
	add("--ro-bind", path.Join(l.SkelDir, "group"), "/etc/group")
	add("--ro-bind", path.Join(l.SkelDir, "resolv.conf"), "/etc/resolv.conf")

	if p.Tier == TierFilesystem {
		add("--ro-bind", "/proc", "/proc")
		add("--cap-drop", "ALL")
	} else {
		add("--proc", "/proc")
	}
	add("--dev", "/dev")
	add("--tmpfs", "/tmp")
	add("--tmpfs", "/run")
	add("--tmpfs", "/home")

	add("--ro-bind", path.Join(l.BinDir, "jdix-init"), l.InitTarget)
	// Must come after --tmpfs /run, or the tmpfs would cover the socket dir.
	add("--bind", l.IPCDir, l.IPCMount)

	// ── tenant-declared filesystem ───────────────────────────────────────
	ws := workspacePath(spec)
	add("--bind", l.WorkspaceDir, ws)

	for _, m := range orderedMounts(spec.Mounts) {
		src := path.Join(p.Volumes[m.Source.Volume], m.Source.SubPath)
		if fd, ok := p.SourceFDs[m.Path]; ok {
			if m.ReadOnly {
				add("--ro-bind-fd", itoa(fd), m.Path)
			} else {
				add("--bind-fd", itoa(fd), m.Path)
			}
		} else if p.Tier == TierFilesystem {
			return nil, fmt.Errorf("mount %s: source directory has not been pinned", m.Path)
		} else if m.ReadOnly {
			add("--ro-bind", src, m.Path)
		} else {
			add("--bind", src, m.Path)
		}
	}

	for _, h := range spec.Hide {
		if p.isDir(h) {
			add("--tmpfs", h)
		} else {
			// Binding host /dev/null over a file blanks it without needing a
			// writable mount point.
			add("--ro-bind", "/dev/null", h)
		}
	}

	// ── process setup ────────────────────────────────────────────────────
	add("--chdir", ws)
	add("--clearenv")
	add("--setenv", "HOME", "/home")
	add("--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	add("--setenv", "TERM", "xterm-256color")
	add("--setenv", "JDIX_SANDBOX", "1")

	add("--die-with-parent") // execd dies -> the whole namespace goes with it
	add("--new-session")     // detach from execd's controlling terminal (TIOCSTI)
	if p.Tier != TierFilesystem {
		add("--as-pid-1")
	} // jdix-init becomes PID 1 and does the reaping

	add("--", l.InitTarget,
		"--socket", path.Join(l.IPCMount, InitSocketName),
		"--workspace", ws)
	if p.Tier != TierUserns && p.Tier != TierFilesystem {
		// No user namespace: jdix-init must drop privileges after mounting.
		add("--drop-to", itoa(p.UID)+":"+itoa(p.GID))
	}
	if p.Tier == TierFilesystem {
		a = append(a, "--subreaper")
	}
	return a, nil
}

// isDir decides how a spec.hide entry is blanked. Policy.IsDir lets tests drive
// this without touching the filesystem.
func (p Policy) isDir(target string) bool {
	if p.IsDirFunc != nil {
		return p.IsDirFunc(target)
	}
	fi, err := os.Stat(target)
	return err == nil && fi.IsDir()
}

// orderedMounts returns the mounts with parents ahead of their children.
//
// bwrap applies binds in argv order and a later bind covers an earlier one, so
// a child listed before its parent would be mounted and then hidden — silently,
// with the parent's own content showing through where the child should be. The
// tenant's order carries no meaning, so it is replaced with one that does.
//
// Depth first, then the path itself: a child is always strictly deeper than its
// parent, and the tiebreak keeps the argv stable for a given spec, which
// matters because it feeds the template hash.
func orderedMounts(mounts []api.Mount) []api.Mount {
	out := make([]api.Mount, len(mounts))
	copy(out, mounts)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := strings.Count(out[i].Path, "/"), strings.Count(out[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
