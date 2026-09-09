//go:build !linux

package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openBeneath is the non-Linux implementation. Production runs on Linux and
// uses openat2; this exists so the package and its tests build and run on a
// developer machine.
//
// It walks the path lstat-ing every prefix and refuses any symlink, then opens
// with O_NOFOLLOW. That is not atomic against a concurrent rename — which is
// precisely why it is not the production path.
func openBeneath(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return os.OpenFile(realRoot, os.O_RDONLY, 0)
	}

	// Every segment *except the last* must already exist and must not be a
	// symlink. The last one is left to the open itself: with O_CREATE it may
	// legitimately not exist yet, and O_NOFOLLOW still refuses a symlink there.
	segs := strings.Split(rel, "/")
	cur := realRoot
	for i, seg := range segs {
		cur = filepath.Join(cur, seg)
		if i == len(segs)-1 {
			break
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, ErrEscape
		}
	}
	if !strings.HasPrefix(cur, realRoot+string(filepath.Separator)) {
		return nil, ErrEscape
	}
	f, err := os.OpenFile(cur, flags|syscallNoFollow, perm)
	if err != nil {
		// O_NOFOLLOW surfaces a symlink as ELOOP; report it as an escape so the
		// caller answers 403 rather than a confusing 500.
		if errors.Is(err, syscall.ELOOP) {
			return nil, ErrEscape
		}
		return nil, err
	}
	return f, nil
}
