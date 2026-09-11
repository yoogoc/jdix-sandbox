package bwrap

import (
	"strings"
	"testing"

	"jdix.io/sandbox/pkg/api"
)

// mountTargets walks a generated argv and returns every path the sandbox will
// have mounted, paired with its source where there is one.
func mountTargets(argv []string) (targets []string, binds [][2]string) {
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--bind", "--ro-bind", "--ro-bind-try", "--bind-try", "--dev-bind":
			if i+2 < len(argv) {
				binds = append(binds, [2]string{argv[i+1], argv[i+2]})
				targets = append(targets, argv[i+2])
				i += 2
			}
		case "--tmpfs", "--proc", "--dev", "--dir":
			if i+1 < len(argv) {
				targets = append(targets, argv[i+1])
				i++
			}
		case "--":
			return
		}
	}
	return
}

func TestGenerateShape(t *testing.T) {
	spec := api.FilesystemSpec{
		Mounts: []api.Mount{{
			Path:     "/data/corpus",
			Source:   api.MountSource{Volume: "corpus", SubPath: "2026-09"},
			ReadOnly: true,
		}},
		Hide: []string{"/etc/hosts"},
	}
	argv, err := Generate(spec, testPolicy(), DefaultLayout())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"--unshare-user", "--unshare-pid", "--as-pid-1", "--die-with-parent", "--new-session",
		"--uid 1000", "--gid 1000",
		"--ro-bind /var/lib/jdix/vol/corpus/2026-09 /data/corpus",
		"--bind /var/lib/jdix/workspace /workspace",
		"--bind /var/lib/jdix/ipc /run/jdix",
		"--ro-bind /dev/null /etc/hosts",
		"--chdir /workspace",
		"-- /jdix-init --socket /run/jdix/init.sock --workspace /workspace",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q\ngot: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--unshare-net") {
		t.Error("--unshare-net must never be emitted; the sandbox needs egress")
	}

	// --clearenv must precede every --setenv, or the sandbox inherits execd's
	// environment, which holds the control-plane token.
	clear, firstSet := -1, -1
	for i, a := range argv {
		if a == "--clearenv" && clear < 0 {
			clear = i
		}
		if a == "--setenv" && firstSet < 0 {
			firstSet = i
		}
	}
	if clear < 0 {
		t.Fatal("--clearenv missing")
	}
	if firstSet >= 0 && firstSet < clear {
		t.Fatalf("--setenv at %d precedes --clearenv at %d", firstSet, clear)
	}

	// /run must be tmpfs'd before the IPC dir is bound into it.
	tmpfsRun, bindIPC := -1, -1
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--tmpfs" && argv[i+1] == "/run" {
			tmpfsRun = i
		}
		if argv[i] == "--bind" && i+2 < len(argv) && argv[i+2] == "/run/jdix" {
			bindIPC = i
		}
	}
	if tmpfsRun < 0 || bindIPC < 0 || tmpfsRun > bindIPC {
		t.Fatalf("--tmpfs /run (%d) must come before --bind ... /run/jdix (%d)", tmpfsRun, bindIPC)
	}
}

func TestGenerateTierDifferences(t *testing.T) {
	p := testPolicy()
	p.Tier = TierCapAdmin
	argv, err := Generate(api.FilesystemSpec{}, p, DefaultLayout())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--unshare-user") {
		t.Error("capadmin tier must not ask bwrap for a user namespace")
	}
	if strings.Contains(joined, "--uid ") {
		t.Error("bwrap --uid requires --unshare-user; capadmin must drop privileges in jdix-init")
	}
	if !strings.Contains(joined, "--drop-to 1000:1000") {
		t.Error("capadmin tier must tell jdix-init to drop privileges")
	}
}

func TestGenerateRejectsInvalidSpec(t *testing.T) {
	if _, err := Generate(mountSpec("/opt/jdix/bin", "corpus", ""), testPolicy(), DefaultLayout()); err == nil {
		t.Fatal("Generate must not emit argv for a spec that fails validation")
	}
}

// FuzzGenerate asserts the invariant the whole isolation story rests on: for
// ANY input the validator accepts, no emitted mount target overlaps a platform
// path, and every bind source stays inside a declared volume (or is platform
// state). DESIGN.md §03.4 calls for exactly this.
func FuzzGenerate(f *testing.F) {
	seeds := []string{
		"/data/x", "/workspace/sub", "/usr/local/x", "/", "/proc", "/opt", "/opt/jdix",
		"/var/lib/jdix", "/run/jdix", "/jdix-init", "/etc/passwd", "/a/../b", "//x", "/a/./b",
	}
	subs := []string{"", "2026-09", "../..", "/abs", "a//b", "a/../b", "\x00"}
	for _, s := range seeds {
		for _, sub := range subs {
			f.Add(s, sub, "corpus", "/etc/hosts")
		}
	}

	l := DefaultLayout()
	p := testPolicy()
	protected := p.protectedTargets(l)
	volRoot := p.Volumes["corpus"]

	f.Fuzz(func(t *testing.T, target, sub, vol, hide string) {
		spec := api.FilesystemSpec{
			Mounts: []api.Mount{{Path: target, Source: api.MountSource{Volume: vol, SubPath: sub}}},
			Hide:   []string{hide},
		}
		argv, err := Generate(spec, p, l)
		if err != nil {
			return // rejected: nothing to check
		}
		_, binds := mountTargets(argv)

		// The tenant's target may not overlap anything the platform owns.
		for _, prot := range protected {
			if overlaps(target, prot) {
				t.Fatalf("accepted target %q overlaps protected %q\nargv: %v", target, prot, argv)
			}
		}

		// Locate the tenant's own bind by its source, which must be rooted in
		// the declared volume. Matching on the target instead would confuse it
		// with the hide entry when the two name the same path.
		var found bool
		for _, b := range binds {
			src, tgt := b[0], b[1]
			if !contains(volRoot, src) {
				continue
			}
			found = true
			if tgt != target {
				t.Fatalf("volume bind landed on %q, expected %q", tgt, target)
			}
			// A path *segment* equal to ".." is a traversal; a segment merely
			// containing dots (say "0..") is an ordinary filename.
			for _, seg := range strings.Split(src, "/") {
				if seg == ".." {
					t.Fatalf("tenant mount source %q contains a traversal segment", src)
				}
			}
		}
		if !found {
			t.Fatalf("accepted spec produced no bind from volume root %q\nargv: %v", volRoot, argv)
		}
	})
}

// Filesystem mode is a distinct capability, not a rung on the legacy ladder:
// it keeps the user namespace that makes unprivileged mounts possible and drops
// the PID namespace that forced procMount=Unmasked, which forced
// hostUsers=false, which made the runtime idmap every volume and put NFS out of
// reach (docs/NFS-CSI-FILESYSTEM-ISOLATION.md §2).
func TestFilesystemModeArgv(t *testing.T) {
	l := DefaultLayout()
	p := DefaultPolicy(TierFilesystem)
	p.Volumes = map[string]string{"corpus": "/var/lib/jdix/vol/corpus"}
	p.IsDirFunc = func(string) bool { return false }
	p.SourceFDs = map[string]int{"/data": 3}

	argv, err := Generate(api.FilesystemSpec{
		Workspace: api.Workspace{Path: "/workspace"},
		Mounts: []api.Mount{{
			Path: "/data", Source: api.MountSource{Volume: "corpus", SubPath: "tenant-a"}, ReadOnly: true,
		}},
	}, p, l)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"--unshare-user",        // mounts without privilege still need this
		"--ro-bind /proc /proc", // the container's masked procfs, not a fresh one
		"--ro-bind-fd 3 /data",  // the pinned directory, never a re-walked path
		"--cap-drop ALL",
		"--subreaper", // jdix-init is no longer PID 1 and must adopt explicitly
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q:\n  %s", want, joined)
		}
	}
	for _, unwanted := range []string{"--unshare-pid", "--as-pid-1", "--proc /proc", "--unshare-uts"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("argv still carries %q, which is what forced hostUsers=false:\n  %s", unwanted, joined)
		}
	}
}

// A mount whose source was never pinned must fail rather than silently fall
// back to path resolution, which a concurrent rename on NFS can redirect.
func TestFilesystemModeRefusesAnUnpinnedSource(t *testing.T) {
	l := DefaultLayout()
	p := DefaultPolicy(TierFilesystem)
	p.Volumes = map[string]string{"corpus": "/var/lib/jdix/vol/corpus"}
	p.IsDirFunc = func(string) bool { return false }

	_, err := Generate(api.FilesystemSpec{
		Workspace: api.Workspace{Path: "/workspace"},
		Mounts:    []api.Mount{{Path: "/data", Source: api.MountSource{Volume: "corpus"}}},
	}, p, l)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("err = %v, want a refusal naming the unpinned source", err)
	}
}

// The legacy tiers are a ladder; filesystem mode is beside it. Treating it as a
// rank would let a node measuring "userns" satisfy a template asking for the
// new mode, or the reverse — either way a silent substitution of one isolation
// model for another.
func TestFilesystemTierIsNotComparableWithTheLadder(t *testing.T) {
	cases := []struct {
		have, min Tier
		want      bool
	}{
		{TierFilesystem, TierFilesystem, true},
		{TierFilesystem, TierUserns, false},
		{TierUserns, TierFilesystem, false},
		{TierChroot, TierFilesystem, false},
		// the ladder itself is unchanged
		{TierUserns, TierChroot, true},
		{TierChroot, TierUserns, false},
	}
	for _, tc := range cases {
		if got := tc.have.AtLeast(tc.min); got != tc.want {
			t.Errorf("Tier(%q).AtLeast(%q) = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

// A writable directory holding read-only ones is a shape tenants need — an
// agent's session directory, say, where it may create new sessions but not
// rewrite old ones. bwrap applies binds in order and a later bind covers an
// earlier one, so the argv has to put parents first however the request was
// written.
func TestNestedMountsAreOrderedParentFirst(t *testing.T) {
	const sessions = "/workspace/users/u/.pi/agent/sessions"
	l := DefaultLayout()
	p := DefaultPolicy(TierFilesystem)
	p.Volumes = map[string]string{"v": "/var/lib/jdix/vol/v"}
	p.IsDirFunc = func(string) bool { return false }

	// Deliberately written child-first, which is how a generated request often
	// comes out.
	mounts := []api.Mount{
		{Path: sessions + "/--projects-2173--", Source: api.MountSource{Volume: "v", SubPath: "s/2173"}, ReadOnly: true},
		{Path: sessions + "/--projects-2334--", Source: api.MountSource{Volume: "v", SubPath: "s/2334"}, ReadOnly: true},
		{Path: sessions, Source: api.MountSource{Volume: "v", SubPath: "s"}},
		{Path: "/workspace/projects/2490", Source: api.MountSource{Volume: "v", SubPath: "p/2490"}},
	}
	p.SourceFDs = map[string]int{}
	for i, m := range mounts {
		p.SourceFDs[m.Path] = 3 + i
	}
	argv, err := Generate(api.FilesystemSpec{
		Workspace: api.Workspace{Path: "/workspace"}, Mounts: mounts,
	}, p, l)
	if err != nil {
		t.Fatal(err)
	}

	at := func(target string) int {
		for i, a := range argv {
			if a == target && i > 0 && strings.HasSuffix(argv[i-2], "-fd") {
				return i
			}
		}
		t.Fatalf("%s is not mounted at all:\n  %s", target, strings.Join(argv, " "))
		return -1
	}
	parent := at(sessions)
	for _, child := range []string{sessions + "/--projects-2173--", sessions + "/--projects-2334--"} {
		if at(child) < parent {
			t.Errorf("%s is bound before its parent %s, so the parent would cover it", child, sessions)
		}
	}
	// The workspace itself is bound before anything nested inside it, or the
	// same problem applies one level up.
	for i, a := range argv {
		if a == "/workspace" && argv[i-2] == "--bind" {
			if i > parent {
				t.Error("the workspace bind comes after a mount nested inside it")
			}
			break
		}
	}
}

// Ordering must not depend on how the request happened to be written: the argv
// feeds the template hash, and an unstable one would roll the warm pool for no
// reason.
func TestMountOrderIsStableAcrossRequestOrder(t *testing.T) {
	l := DefaultLayout()
	mk := func(mounts []api.Mount) string {
		p := DefaultPolicy(TierFilesystem)
		p.Volumes = map[string]string{"v": "/var/lib/jdix/vol/v"}
		p.IsDirFunc = func(string) bool { return false }
		p.SourceFDs = map[string]int{}
		for _, m := range mounts {
			p.SourceFDs[m.Path] = 3
		}
		argv, err := Generate(api.FilesystemSpec{
			Workspace: api.Workspace{Path: "/workspace"}, Mounts: mounts,
		}, p, l)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(argv, " ")
	}
	a := []api.Mount{
		{Path: "/workspace/b/x", Source: api.MountSource{Volume: "v", SubPath: "1"}},
		{Path: "/workspace/a", Source: api.MountSource{Volume: "v", SubPath: "2"}},
		{Path: "/workspace/b", Source: api.MountSource{Volume: "v", SubPath: "3"}},
	}
	b := []api.Mount{a[2], a[0], a[1]}
	if mk(a) != mk(b) {
		t.Error("the same mounts written in a different order produce a different argv")
	}
}
