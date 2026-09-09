package safepath

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildTree lays out a root containing an ordinary file, a directory, and a
// selection of symlinks an attacker might plant.
func buildTree(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "ok.txt"), "inside")
	write(filepath.Join(root, "sub", "deep.txt"), "deep")
	write(filepath.Join(outside, "secret.txt"), "SECRET")

	links := [][2]string{
		{filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape-file")},
		{outside, filepath.Join(root, "escape-dir")},
		{"/etc/passwd", filepath.Join(root, "abs-link")},
		{"../outside/secret.txt", filepath.Join(root, "rel-link")},
		{filepath.Join(root, "ok.txt"), filepath.Join(root, "inward-link")},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

func TestOpenBeneathReadsContainedFiles(t *testing.T) {
	root, _ := buildTree(t)
	for _, rel := range []string{"ok.txt", "sub/deep.txt"} {
		f, err := OpenBeneath(root, rel, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if len(b) == 0 {
			t.Fatalf("%s: read nothing", rel)
		}
	}
}

func TestOpenBeneathRefusesSymlinks(t *testing.T) {
	root, _ := buildTree(t)
	// Every symlink is refused, including one pointing back inside the root.
	// Allowing "harmless" links would mean resolving them, and a resolver is
	// exactly what an attacker races against.
	for _, rel := range []string{"escape-file", "escape-dir/secret.txt", "abs-link", "rel-link", "inward-link"} {
		f, err := OpenBeneath(root, rel, os.O_RDONLY, 0)
		if err == nil {
			f.Close()
			t.Fatalf("%s: expected refusal, got a readable file", rel)
		}
		if !errors.Is(err, ErrEscape) && !os.IsNotExist(err) && !errors.Is(err, ErrBadPath) {
			// A symlinked *directory* component may surface as ENOTDIR/ELOOP
			// depending on the platform; what matters is that it failed.
			t.Logf("%s refused with %v", rel, err)
		}
	}
}

func TestOpenBeneathRejectsMalformedRelatives(t *testing.T) {
	root, _ := buildTree(t)
	for _, rel := range []string{"/etc/passwd", "../outside/secret.txt", "sub/../../outside", "a//b", "a/./b", "x\x00y"} {
		if _, err := OpenBeneath(root, rel, os.O_RDONLY, 0); !errors.Is(err, ErrBadPath) {
			t.Fatalf("%q: expected ErrBadPath, got %v", rel, err)
		}
	}
}

func TestOpenBeneathMissingFileIsNotAnEscape(t *testing.T) {
	root, _ := buildTree(t)
	_, err := OpenBeneath(root, "nope.txt", os.O_RDONLY, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsEscape(err) {
		t.Fatalf("a missing file must not be reported as an escape: %v", err)
	}
}

func TestResolveBeneath(t *testing.T) {
	root, _ := buildTree(t)
	got, err := ResolveBeneath(root, "sub/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "sub/deep.txt"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := ResolveBeneath(root, "escape-file"); err == nil {
		t.Fatal("expected refusal for a symlinked source")
	}
}

// FuzzOpenBeneath asserts the one property that matters: whatever the relative
// path, an opened file is always inside the root.
func FuzzOpenBeneath(f *testing.F) {
	for _, s := range []string{"ok.txt", "sub/deep.txt", "escape-file", "../x", "/abs", "a//b", "..", ".", ""} {
		f.Add(s)
	}
	root, outside := buildTree(&testing.T{})
	realRoot, _ := filepath.EvalSymlinks(root)
	realOutside, _ := filepath.EvalSymlinks(outside)

	f.Fuzz(func(t *testing.T, rel string) {
		file, err := OpenBeneath(root, rel, os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer file.Close()
		name, err := filepath.EvalSymlinks(file.Name())
		if err != nil {
			return
		}
		if realOutside != "" && (name == realOutside || filepath.Dir(name) == realOutside) {
			t.Fatalf("opened %q which lives outside the root", name)
		}
		if rel != "" && rel != "." && !filepath.IsLocal(mustRel(t, realRoot, name)) {
			t.Fatalf("opened %q which is not inside %q", name, realRoot)
		}
	})
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	r, err := filepath.Rel(base, target)
	if err != nil {
		return ".."
	}
	return r
}
