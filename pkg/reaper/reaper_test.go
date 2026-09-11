package reaper

import (
	"os/exec"
	"testing"
	"time"
)

func TestReaperDeliversExitStatus(t *testing.T) {
	r := NewReaper()
	stop := make(chan struct{})
	go r.Run(stop)
	defer close(stop)

	cases := []struct {
		args []string
		want int
	}{
		{[]string{"/bin/sh", "-c", "exit 0"}, 0},
		{[]string{"/bin/sh", "-c", "exit 7"}, 7},
		{[]string{"/bin/sh", "-c", "kill -TERM $$"}, 128 + 15},
	}
	for _, tc := range cases {
		cmd := exec.Command(tc.args[0], tc.args[1:]...)
		pid, err := r.StartCmd(cmd)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan int, 1)
		go func() { done <- r.Wait(pid) }()
		select {
		case got := <-done:
			if got != tc.want {
				t.Errorf("%v: got status %d want %d", tc.args, got, tc.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: Wait did not return; the reaper lost the status", tc.args)
		}
	}
}

// A process that exits before Wait is called must not lose its status: that is
// the race the pending map exists for.
func TestReaperHandlesExitBeforeWait(t *testing.T) {
	r := NewReaper()
	stop := make(chan struct{})
	go r.Run(stop)
	defer close(stop)

	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	pid, err := r.StartCmd(cmd)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let it exit and be reaped first

	done := make(chan int, 1)
	go func() { done <- r.Wait(pid) }()
	select {
	case got := <-done:
		if got != 3 {
			t.Fatalf("got %d want 3", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status lost when the child exited before Wait")
	}
}

func TestReaperCollectsOrphans(t *testing.T) {
	r := NewReaper()
	stop := make(chan struct{})
	go r.Run(stop)
	defer close(stop)

	// The shell exits immediately while its child lingers, so the child is
	// reparented. We only assert that reaping does not deadlock or panic; who
	// inherits the orphan depends on whether we are really PID 1.
	cmd := exec.Command("/bin/sh", "-c", "( sleep 0.2 & ) ; exit 0")
	pid, err := r.StartCmd(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Wait(pid); got != 0 {
		t.Fatalf("got %d want 0", got)
	}
	time.Sleep(500 * time.Millisecond)
	r.reap() // must be safe to call with nothing left to collect
}

// pending exists for one narrow race, not as storage. jdix-init is the
// subreaper for everything a sandbox starts, and a sandbox that never expires
// starts them indefinitely — an unbounded map would leak with no symptom until
// the Pod is OOM-killed.
func TestPendingStatusesAreBounded(t *testing.T) {
	r := NewReaper()
	for pid := 1000; pid < 1000+maxPending*3; pid++ {
		r.deliver(pid, 0)
	}
	r.mu.Lock()
	n, ordered := len(r.pending), len(r.order)
	r.mu.Unlock()
	if n > maxPending || ordered > maxPending {
		t.Fatalf("pending=%d order=%d, want at most %d", n, ordered, maxPending)
	}
	// The oldest are the ones dropped; the most recent must still be claimable,
	// because those are the ones a caller could still be racing to Adopt.
	if got := r.Wait(1000 + maxPending*3 - 1); got != 0 {
		t.Errorf("the newest status was evicted: got %d", got)
	}
}

// A claimed status must give up its slot, or claimed pids would go on occupying
// room in the ring and evict statuses that are still wanted.
func TestClaimingAStatusFreesItsSlot(t *testing.T) {
	r := NewReaper()
	r.deliver(4242, 7)
	if got := r.Wait(4242); got != 7 {
		t.Fatalf("Wait = %d, want 7", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) != 0 || len(r.order) != 0 {
		t.Errorf("claimed status left behind: pending=%v order=%v", r.pending, r.order)
	}
}
