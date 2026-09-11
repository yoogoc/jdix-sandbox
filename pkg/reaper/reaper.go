package reaper

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// Reaper is the single owner of wait4 in a process that inherits orphans.
//
// Two processes need it. jdix-init is the subreaper for everything the sandbox
// starts; jdix-execd is the container's PID 1 and collects whatever outlives
// jdix-init. Both must reap or the process table fills with zombies.
//
// The obvious implementation — a background loop calling wait4(-1) — races with
// os/exec, which waits on its own children: whichever gets there first, the
// other sees ECHILD and the exit status is lost. So nothing else waits. Callers
// use StartCmd/Adopt/Wait instead of cmd.Wait(), and this type delivers
// statuses to them. Orphans nobody is waiting for are discarded, which is
// exactly what an inheriting process should do with them.
// maxPending bounds the statuses held for processes nobody has asked about.
//
// pending exists for one narrow race: a child started outside StartCmd can exit
// before Adopt registers it. That window is microseconds, so a small ring is
// ample. Leaving it unbounded used to look safe — jdix-init only saw its own
// API-started children — but it is now the subreaper for every orphan a sandbox
// produces, and a sandbox that never expires produces them indefinitely. An
// unbounded map would be a slow leak with no upper limit and no symptom until
// the Pod is OOM-killed.
const maxPending = 256

type Reaper struct {
	mu      sync.Mutex
	waiters map[int]chan int
	pending map[int]int // exited before the caller asked
	// order records insertion order so the oldest status is evicted first. A
	// status nobody claimed within the next 255 exits was never going to be.
	order []int
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
		r.forget(pid)
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
	// briefly, evicting the oldest so a long-lived sandbox cannot grow this
	// without limit.
	if _, dup := r.pending[pid]; !dup {
		r.order = append(r.order, pid)
	}
	r.pending[pid] = code
	for len(r.order) > maxPending {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.pending, oldest)
	}
}

// forget drops a claimed status and its eviction slot together, so a claimed
// pid does not go on occupying room in the ring.
func (r *Reaper) forget(pid int) {
	delete(r.pending, pid)
	for i, p := range r.order {
		if p == pid {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// waitStatusCode reports a signalled process as 128+signal, the same convention
// a shell uses, so callers need no special case for "killed".
func waitStatusCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
