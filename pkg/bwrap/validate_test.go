package bwrap

import (
	"errors"
	"strings"
	"testing"

	"jdix.io/sandbox/pkg/api"
)

func testPolicy() Policy {
	p := DefaultPolicy(TierUserns)
	p.Volumes = map[string]string{"corpus": "/var/lib/jdix/vol/corpus"}
	p.IsDirFunc = func(string) bool { return false }
	return p
}

func mountSpec(target, vol, sub string) api.FilesystemSpec {
	return api.FilesystemSpec{
		Mounts: []api.Mount{{Path: target, Source: api.MountSource{Volume: vol, SubPath: sub}, ReadOnly: true}},
	}
}

func TestValidateRejectsHostileSpecs(t *testing.T) {
	l := DefaultLayout()
	cases := []struct {
		name string
		spec api.FilesystemSpec
		want string // substring of the reason
	}{
		{"traversal in target", mountSpec("/data/../../etc", "corpus", ""), "canonical"},
		{"dot segment in target", mountSpec("/data/./x", "corpus", ""), "canonical"},
		{"double slash in target", mountSpec("/data//x", "corpus", ""), "canonical"},
		{"relative target", mountSpec("data/x", "corpus", ""), "absolute"},
		{"root target", mountSpec("/", "corpus", ""), "root directory"},
		{"NUL in target", mountSpec("/data\x00/x", "corpus", ""), "absolute"},

		{"target is /proc", mountSpec("/proc", "corpus", ""), "overlaps platform path /proc"},
		{"target inside /proc", mountSpec("/proc/self", "corpus", ""), "overlaps platform path /proc"},
		{"target is /dev", mountSpec("/dev", "corpus", ""), "overlaps platform path /dev"},
		{"target is platform root", mountSpec("/opt/jdix", "corpus", ""), "overlaps platform path /opt/jdix"},
		{"target shadows platform root", mountSpec("/opt", "corpus", ""), "overlaps platform path /opt/jdix"},
		{"target inside platform root", mountSpec("/opt/jdix/bin", "corpus", ""), "overlaps platform path /opt/jdix"},
		{"target is ipc mount", mountSpec("/run/jdix", "corpus", ""), "overlaps platform path /run/jdix"},
		{"target is init binary", mountSpec("/jdix-init", "corpus", ""), "overlaps platform path /jdix-init"},
		{"target is state root", mountSpec("/var/lib/jdix", "corpus", ""), "overlaps platform path /var/lib/jdix"},
		{"target shadows state root", mountSpec("/var/lib", "corpus", ""), "overlaps platform path /var/lib/jdix"},
		{"target is /etc/passwd", mountSpec("/etc/passwd", "corpus", ""), "overlaps platform path /etc/passwd"},

		{"target replaces /usr", mountSpec("/usr", "corpus", ""), "would replace /usr"},
		{"target replaces workspace", mountSpec("/workspace", "corpus", ""), "would replace /workspace"},

		{"undeclared volume", mountSpec("/data/x", "secrets", ""), "not declared"},
		{"empty volume", mountSpec("/data/x", "", ""), "must name a volume"},
		{"absolute subPath", mountSpec("/data/x", "corpus", "/etc"), "relative to the volume root"},
		{"traversal subPath", mountSpec("/data/x", "corpus", "../../etc"), "'..' segments"},
		{"non-canonical subPath", mountSpec("/data/x", "corpus", "a//b"), "canonical form"},
		{"NUL subPath", mountSpec("/data/x", "corpus", "a\x00b"), "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testPolicy().Validate(tc.spec, l)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected *ValidationError, got %T", err)
			}
			if !strings.Contains(ve.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", ve.Reason, tc.want)
			}
		})
	}
}

func TestValidateAcceptsReasonableSpecs(t *testing.T) {
	l := DefaultLayout()
	cases := []struct {
		name string
		spec api.FilesystemSpec
	}{
		{"empty spec", api.FilesystemSpec{}},
		{"simple ro mount", mountSpec("/data/corpus", "corpus", "2026-09")},
		{"mount inside /usr is allowed", mountSpec("/usr/local/share/corpus", "corpus", "")},
		{"mount inside workspace is allowed", mountSpec("/workspace/data", "corpus", "")},
		{"custom workspace", api.FilesystemSpec{Workspace: api.Workspace{Path: "/srv/work"}}},
		{"subset of system paths", api.FilesystemSpec{AllowSystemPaths: []string{"/usr", "/bin"}}},
		{"hide a file", api.FilesystemSpec{Hide: []string{"/etc/hosts"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := testPolicy().Validate(tc.spec, l); err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

func TestValidateDuplicateAndLimits(t *testing.T) {
	l := DefaultLayout()
	p := testPolicy()

	dup := api.FilesystemSpec{Mounts: []api.Mount{
		{Path: "/data/x", Source: api.MountSource{Volume: "corpus"}},
		{Path: "/data/x", Source: api.MountSource{Volume: "corpus"}},
	}}
	if err := p.Validate(dup, l); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}

	var many api.FilesystemSpec
	for i := 0; i < MaxMounts+1; i++ {
		many.Mounts = append(many.Mounts, api.Mount{
			Path:   "/data/" + itoa(i),
			Source: api.MountSource{Volume: "corpus"},
		})
	}
	if err := p.Validate(many, l); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("expected mount-count rejection, got %v", err)
	}
}

func TestAllowSystemPathsCannotBeExtended(t *testing.T) {
	spec := api.FilesystemSpec{AllowSystemPaths: []string{"/usr", "/etc"}}
	err := testPolicy().Validate(spec, DefaultLayout())
	if err == nil || !strings.Contains(err.Error(), "not in the platform allowlist") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
}

func TestChrootTierRejectsMounts(t *testing.T) {
	p := testPolicy()
	p.Tier = TierChroot
	err := p.Validate(mountSpec("/data/x", "corpus", ""), DefaultLayout())
	if err == nil || !strings.Contains(err.Error(), "chroot") {
		t.Fatalf("expected tier rejection, got %v", err)
	}
	// A spec without mounts is still fine on a chroot node.
	if err := p.Validate(api.FilesystemSpec{}, DefaultLayout()); err != nil {
		t.Fatalf("unexpected rejection on chroot tier: %v", err)
	}
}

func TestTierRank(t *testing.T) {
	if !TierUserns.AtLeast(TierCapAdmin) || !TierCapAdmin.AtLeast(TierChroot) {
		t.Fatal("tier ordering is wrong")
	}
	if TierChroot.AtLeast(TierUserns) {
		t.Fatal("chroot must not satisfy a userns requirement")
	}
	if Tier("bogus").AtLeast(TierChroot) {
		t.Fatal("an unknown tier must not satisfy anything")
	}
}

func TestHideMayNotShadowAMount(t *testing.T) {
	l := DefaultLayout()
	p := testPolicy()

	// Exactly the case the fuzzer found: mount and hide naming the same path.
	spec := api.FilesystemSpec{
		Mounts: []api.Mount{{Path: "/data/x", Source: api.MountSource{Volume: "corpus"}}},
		Hide:   []string{"/data/x"},
	}
	if err := p.Validate(spec, l); err == nil || !strings.Contains(err.Error(), "would hide") {
		t.Fatalf("expected hide/mount conflict rejection, got %v", err)
	}

	// An ancestor of a mount target is the same problem.
	spec.Hide = []string{"/data"}
	if err := p.Validate(spec, l); err == nil || !strings.Contains(err.Error(), "would hide") {
		t.Fatalf("expected ancestor hide rejection, got %v", err)
	}

	// Hiding a path *inside* a mount is meaningful and stays allowed.
	spec.Hide = []string{"/data/x/secret.env"}
	if err := p.Validate(spec, l); err != nil {
		t.Fatalf("hiding inside a mount should be allowed, got %v", err)
	}

	// Hiding the workspace wholesale is rejected too.
	if err := p.Validate(api.FilesystemSpec{Hide: []string{"/workspace"}}, l); err == nil {
		t.Fatal("expected workspace hide rejection")
	}
}
