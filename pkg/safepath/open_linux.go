//go:build linux

package safepath

import (
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// openat2 constants. Declared here rather than pulled from golang.org/x/sys so
// the isolation-critical packages stay dependency-free.
const (
	sysOpenat2 = 437

	resolveNoMagiclinks = 0x02
	resolveBeneath      = 0x08
)

type openHow struct {
	Flags   uint64
	Mode    uint64
	Resolve uint64
}

// openat2Missing latches once we learn the kernel is older than 5.6, so we stop
// paying for a failing syscall on every open.
var openat2Missing atomic.Bool

func openBeneath(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	if !openat2Missing.Load() {
		f, err, supported := openBeneathAt2(root, rel, flags, perm)
		if supported {
			return f, err
		}
		openat2Missing.Store(true)
	}
	return openBeneathWalk(root, rel, flags, perm)
}

// openBeneathAt2 reports supported=false only when the kernel does not
// implement openat2 at all, so a genuine permission error is not silently
// retried through the weaker fallback.
func openBeneathAt2(root, rel string, flags int, perm os.FileMode) (f *os.File, err error, supported bool) {
	dirFd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}, true
	}
	defer syscall.Close(dirFd)

	p, err := syscall.BytePtrFromString(rel)
	if err != nil {
		return nil, ErrBadPath, true
	}
	how := openHow{
		Flags:   uint64(flags) | syscall.O_CLOEXEC,
		Mode:    uint64(perm.Perm()),
		Resolve: resolveBeneath | resolveNoMagiclinks,
	}
	fd, _, errno := syscall.Syscall6(sysOpenat2, uintptr(dirFd),
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&how)),
		unsafe.Sizeof(how), 0, 0)
	switch errno {
	case 0:
		return os.NewFile(fd, root+"/"+rel), nil, true
	case syscall.ENOSYS, syscall.EPERM:
		// EPERM here usually means a seccomp filter blocks openat2 rather than
		// a real access denial, so fall through to the walk either way.
		return nil, nil, false
	case syscall.EXDEV, syscall.ELOOP:
		// EXDEV is openat2's signal that RESOLVE_BENEATH was violated.
		return nil, ErrEscape, true
	default:
		return nil, &os.PathError{Op: "openat2", Path: rel, Err: errno}, true
	}
}

// openBeneathWalk resolves rel one segment at a time from a descriptor on root,
// refusing to follow any symlink. Used when the kernel predates openat2.
func openBeneathWalk(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	dirFd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() {
		if dirFd >= 0 {
			syscall.Close(dirFd)
		}
	}()

	if rel == "." {
		f := os.NewFile(uintptr(dirFd), root)
		dirFd = -1
		return f, nil
	}

	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		openFlags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		var mode uint32
		if last {
			openFlags = flags | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
			mode = uint32(perm.Perm())
		}
		fd, err := syscall.Openat(dirFd, seg, openFlags, mode)
		if err != nil {
			// O_NOFOLLOW reports a symlink as ELOOP; treat that as an escape
			// attempt rather than an ordinary I/O error.
			if err == syscall.ELOOP {
				return nil, ErrEscape
			}
			return nil, &os.PathError{Op: "openat", Path: seg, Err: err}
		}
		syscall.Close(dirFd)
		dirFd = fd
	}
	f := os.NewFile(uintptr(dirFd), root+"/"+rel)
	dirFd = -1
	return f, nil
}
