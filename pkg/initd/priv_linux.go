//go:build linux

package initd

import (
	"fmt"
	"syscall"
)

// DropPrivileges lowers the process to uid:gid permanently.
//
// Only the capadmin tier needs it: there bwrap has no user namespace to map ids
// with, so jdix-init does the drop itself — after the mounts are in place and
// before any tenant code runs. On the userns tier bwrap has already set the ids
// and this is never called.
func DropPrivileges(uid, gid int) error {
	// Groups go first: once the uid is dropped, the privilege needed to change
	// group membership is gone.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	// syscall.Setgid/Setuid apply to every OS thread, which matters because the
	// Go runtime may run our goroutines on any of them.
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	// Verify rather than trust: a partial drop would leave tenant code running
	// with more privilege than intended, and silently.
	if got := syscall.Geteuid(); got != uid {
		return fmt.Errorf("privilege drop incomplete: euid is %d, want %d", got, uid)
	}
	if got := syscall.Getuid(); got != uid {
		return fmt.Errorf("privilege drop incomplete: ruid is %d, want %d", got, uid)
	}
	return nil
}
