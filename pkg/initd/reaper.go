package initd

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// Reaper is the single owner of wait4 in this process.
//
// As PID 1 inside the namespace, jdix-init inherits every orphan the sandbox
// creates and must reap them or the process table fills with zombies. The
// obvious implementation — a background loop calling wait4(-1) — races with
// os/exec, which waits on its own children: whichever gets there first, the
// other sees ECHILD and the exit status is lost.
//
// So nothing else waits. Callers use StartCmd/Wait instead of cmd.Wait(), and
// this type delivers statuses to them. Orphans nobody is waiting for are simply
// discarded, which is exactly what PID 1 should do with them.
type Reaper struct {
	mu      sync.Mutex
	waiters map[int]chan int
	pending map[int]int // exited before the caller asked
}

func NewReaper() *Reaper {
	return &Reaper{waiters: map[int]chan int{}, pending: map[int]int{}}
}

// Run installs the SIGCHLD handler and reaps until ctx-less shutdown. It also
// sweeps once at startup, in case a child exited during initialisation.
func (r *Reaper) Run(stop <-chan struct{}) {
	ch := make(chan os.Signal, 64)
	signal.Notify(ch, syscall.SIGCHLD)
	defer signal.Stop(ch)

	r.reap()
	for {
		select {
		case <-stop:
			return
		case <-ch:
			r.reap()
		}
	}
}

// StartCmd starts a command and registers interest in its exit status. The lock
// spans both operations so a child that exits instantly cannot have its status
// delivered before there is anywhere to put it.
func (r *Reaper) StartCmd(cmd *exec.Cmd) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	r.waiters[pid] = make(chan int, 1)
	return pid, nil
}

// Adopt registers interest in a process this Reaper did not start.
//
// pty.StartWithSize calls cmd.Start() itself, so the PTY path cannot go through
// StartCmd. Registering afterwards is still safe: if the child exited in the
// gap, its status is sitting in pending and Wait looks there first.
func (r *Reaper) Adopt(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[pid]; ok {
		return // already exited; Wait will find it
	}
	if _, ok := r.waiters[pid]; !ok {
		r.waiters[pid] = make(chan int, 1)
	}
}

// Wait blocks until the process exits and returns a shell-style status.
func (r *Reaper) Wait(pid int) int {
	r.mu.Lock()
	if code, ok := r.pending[pid]; ok {
		delete(r.pending, pid)
		delete(r.waiters, pid)
		r.mu.Unlock()
		return code
	}
	ch, ok := r.waiters[pid]
	r.mu.Unlock()
	if !ok {
		return -1
	}
	code := <-ch

	r.mu.Lock()
	delete(r.waiters, pid)
	r.mu.Unlock()
	return code
}

func (r *Reaper) reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		// pid 0 means "children exist but none has exited"; ECHILD means there
		// are none at all. Either way there is nothing left to collect now.
		if pid <= 0 || err != nil {
			return
		}
		r.deliver(pid, waitStatusCode(ws))
	}
}

func (r *Reaper) deliver(pid, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.waiters[pid]; ok {
		select {
		case ch <- code:
		default:
		}
		return
	}
	// An orphan, or a status that arrived before Wait was called. Remember it
	// briefly; if nobody ever asks, the map entry is the only trace and it is
	// bounded by how many processes the sandbox can start.
	r.pending[pid] = code
}

// waitStatusCode reports a signalled process as 128+signal, the same convention
// a shell uses, so callers need no special case for "killed".
func waitStatusCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
