//go:build linux

package isolation

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// ProtectSupervisor protects all threads sharing the process memory against
// proc/ptrace access. Do this after exec and before reading any credentials.
func ProtectSupervisor() error { return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }

func BecomeSubreaper() error { return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) }

// WorkloadFilter returns a classic BPF seccomp program for bwrap to install
// after its mounts are complete. The node profile supplies the syscall allowlist;
// this second filter removes setup-only permissions from every user descendant.
func WorkloadFilter() (*os.File, error) {
	var arch uint32
	switch runtime.GOARCH {
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	default:
		return nil, fmt.Errorf("filesystem isolation does not support %s", runtime.GOARCH)
	}
	ins := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	deny := func(n int, errno uint32) {
		ins = append(ins, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(n), Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | errno})
	}
	// x32 uses the amd64 audit arch with bit 30 set in the syscall number.
	ins = append(ins, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS})
	for _, n := range []int{unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_MOUNT, unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV,
		unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_OPEN_BY_HANDLE_AT,
		unix.SYS_NAME_TO_HANDLE_AT, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN,
		unix.SYS_FSMOUNT, unix.SYS_FSCONFIG, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR,
		unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_IO_URING_SETUP} {
		deny(n, uint32(unix.EPERM))
	}
	// Let libc fall back to clone for ordinary threads and subprocesses.
	deny(unix.SYS_CLONE3, uint32(unix.ENOSYS))
	ins = append(ins,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE, Jf: 3},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K,
			K: unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	)
	fd, err := unix.MemfdCreate("jdix-workload-seccomp", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "jdix-workload-seccomp")
	if err := binary.Write(f, binary.LittleEndian, ins); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
