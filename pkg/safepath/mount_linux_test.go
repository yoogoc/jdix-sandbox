//go:build linux

package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

// OpenMount is what stands between a tenant's subPath and the rest of a shared
// volume. Validation alone cannot do it: the string is checked once, and on NFS
// a symlink or a concurrent rename can change what that string resolves to
// before bwrap gets round to opening it. So the source is pinned to an fd here
// and never resolved from a path again
// (docs/NFS-CSI-FILESYSTEM-ISOLATION.md §5.2).
func TestOpenMountPinsBeneathTheVolumeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tenant-a"))
	mustMkdir(t, filepath.Join(root, "tenant-a", "nested"))
	mustMkdir(t, filepath.Join(outside, "secrets"))

	// A tenant who can write into their own subtree can plant these.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "tenant-a"), filepath.Join(root, "sideways")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"the volume root itself", "", false},
		{"a plain subdirectory", "tenant-a", false},
		{"a nested subdirectory", "tenant-a/nested", false},
		{"traversal", "../" + filepath.Base(outside), true},
		{"absolute", "/etc", true},
		{"non-canonical", "tenant-a//nested", true},
		{"NUL", "tenant-a\x00/x", true},
		// The symlink cases are the reason this exists at all: both targets are
		// real directories, so only RESOLVE_NO_SYMLINKS refuses them.
		{"symlink out of the root", "escape", true},
		{"symlink staying inside the root", "sideways", true},
		{"a path through a symlink", "escape/secrets", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := OpenMount(root, tc.rel)
			if f != nil {
				defer f.Close()
			}
			if tc.wantErr && err == nil {
				t.Fatalf("OpenMount(%q) succeeded; it must not resolve outside or through links", tc.rel)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("OpenMount(%q): %v", tc.rel, err)
			}
		})
	}
}

// The pin has to survive the directory being renamed out from under it, which
// is the race a path-based mount source loses.
func TestOpenMountSurvivesARenameAfterPinning(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tenant-a"))
	marker := filepath.Join(root, "tenant-a", "marker")
	if err := os.WriteFile(marker, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := OpenMount(root, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Someone swaps the directory for another tenant's after the check.
	mustMkdir(t, filepath.Join(root, "tenant-b"))
	if err := os.Rename(filepath.Join(root, "tenant-a"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "tenant-b"), filepath.Join(root, "tenant-a")); err != nil {
		t.Fatal(err)
	}

	// The fd still names the directory that was checked, not whatever now
	// answers to that path.
	names, err := os.ReadDir("/proc/self/fd/" + itoa(int(f.Fd())))
	if err != nil {
		t.Skipf("cannot read back through the pinned fd here: %v", err)
	}
	found := false
	for _, n := range names {
		if n.Name() == "marker" {
			found = true
		}
	}
	if !found {
		t.Error("the pinned fd followed the rename; a path-based source would have")
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
