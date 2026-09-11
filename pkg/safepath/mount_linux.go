//go:build linux

package safepath

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenMount pins a directory beneath a trusted volume root. Mount sources must
// never fall back to a path walk: a concurrent rename could move an intermediate
// directory outside the root. NO_XDEV also rejects nested mounts in the selection.
func OpenMount(root, relative string) (*os.File, error) {
	rel, err := checkRel(relative)
	if err != nil {
		return nil, err
	}
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)
	fd, err := unix.Openat2(rfd, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, fmt.Errorf("pin mount %s/%s: %w", root, rel, err)
	}
	return os.NewFile(uintptr(fd), root+"/"+rel), nil
}
