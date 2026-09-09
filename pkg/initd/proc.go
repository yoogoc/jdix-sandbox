package initd

import (
	"os"
	"os/exec"
	"sync"
	"time"

	"jdix.io/sandbox/pkg/api"
)

// procTable tracks processes started through the API so /v1/processes can list
// them and signals can be delivered. It deliberately does not scan /proc: the
// sandbox may spawn children we never saw, and reporting a partial view as if
// it were complete would be worse than reporting only what we started.
type procTable struct {
	mu   sync.Mutex
	next int
	m    map[int]*procEntry
}

type procEntry struct {
	cmd     *exec.Cmd
	label   string
	started time.Time
	done    bool
}

func newProcTable() *procTable { return &procTable{m: map[int]*procEntry{}} }

// addPID registers an already-started process. The reaper starts commands, so
// the pid is known before the table sees it.
func (t *procTable) addPID(pid int, cmd *exec.Cmd, label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[pid] = &procEntry{cmd: cmd, label: label, started: time.Now()}
}

func (t *procTable) finish(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.m[pid]; ok {
		e.done = true
	}
}

func (t *procTable) forget(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, pid)
}

// status reports whether a process has exited. It deliberately does not return
// the entry: handing the pointer out would let callers read fields this mutex
// is meant to protect.
func (t *procTable) status(pid int) (done, exists bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[pid]
	if !ok {
		return false, false
	}
	return e.done, true
}

// process returns the OS handle. cmd.Process is written once before the entry
// is published and never changes, so copying it out under the lock is safe.
func (t *procTable) process(pid int) (*os.Process, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[pid]
	if !ok || e.cmd == nil {
		return nil, false
	}
	return e.cmd.Process, true
}

func (t *procTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func (t *procTable) list() []api.Process {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]api.Process, 0, len(t.m))
	for pid, e := range t.m {
		out = append(out, api.Process{
			PID:     pid,
			Cmd:     e.label,
			Started: e.started.UTC().Format(time.RFC3339),
			Running: !e.done,
		})
	}
	return out
}
