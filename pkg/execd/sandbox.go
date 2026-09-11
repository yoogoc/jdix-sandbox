// Package execd is jdix-execd: the container's PID 1, outside the bubblewrap
// namespace.
//
// It owns everything the sandbox itself must not be able to touch — the
// control-plane token, the bwrap binary, the isolation tier measurement — and
// exposes two surfaces: a cluster-internal control plane the controller uses to
// bind a sandbox, and an authenticated data plane that reverse proxies into
// jdix-init over a unix socket.
package execd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
	"jdix.io/sandbox/pkg/isolation"
	"jdix.io/sandbox/pkg/reaper"
	"jdix.io/sandbox/pkg/safepath"
	"strconv"
)

// Launcher starts the sandbox process. Production runs bubblewrap; tests
// substitute a launcher that runs jdix-init directly, which exercises every
// line of the bind path that does not need a kernel namespace.
type Launcher func(ctx context.Context, argv []string, files []*os.File) (*exec.Cmd, error)

// BwrapLauncher is the production launcher.
func BwrapLauncher(bwrapPath string) Launcher {
	return func(ctx context.Context, argv []string, files []*os.File) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, bwrapPath, argv...)
		cmd.ExtraFiles = files
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, cmd.Start()
	}
}

// Config is everything execd needs that does not come from a bind request.
type Config struct {
	Layout    bwrap.Layout
	Tier      bwrap.Tier
	Volumes   map[string]string // template-declared volume name -> Pod mount path
	Launcher  Launcher
	UID, GID  int
	InitDial  time.Duration // how long to wait for jdix-init to answer
	GraceKill time.Duration
}

// sandbox is the bound state. Nil until a bind succeeds.
type sandbox struct {
	id        string
	tenant    string
	token     string
	cmd       *exec.Cmd
	boundAt   time.Time
	expiresAt time.Time
	argv      []string
	cancel    context.CancelFunc
	// reaper is the process that owns wait4 here, or nil when this process does
	// not inherit anything and os/exec may wait for itself.
	reaper   *reaper.Reaper
	ttlTimer *time.Timer
}

// launch builds the argv, starts the sandbox process and waits for jdix-init to
// come up. It returns only once the namespace is usable, so a 200 from
// /internal/v1/bind genuinely means ready (DESIGN.md §07.4).
func (s *Server) launch(ctx context.Context, req api.BindRequest) (*sandbox, error) {
	policy := bwrap.DefaultPolicy(s.cfg.Tier)
	policy.UID, policy.GID = s.cfg.UID, s.cfg.GID
	policy.Volumes = s.cfg.Volumes

	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	if err := policy.Validate(req.Filesystem, s.cfg.Layout); err != nil {
		return nil, err
	}
	if policy.Tier == bwrap.TierFilesystem {
		policy.SourceFDs = map[string]int{}
		for _, m := range req.Filesystem.Mounts {
			f, err := safepath.OpenMount(policy.Volumes[m.Source.Volume], m.Source.SubPath)
			if err != nil {
				return nil, err
			}
			policy.SourceFDs[m.Path] = 3 + len(files)
			files = append(files, f)
		}
	}
	argv, err := bwrap.Generate(req.Filesystem, policy, s.cfg.Layout)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}

	if policy.Tier == bwrap.TierFilesystem {
		filter, err := isolation.WorkloadFilter()
		if err != nil {
			return nil, err
		}
		argv = append([]string{"--seccomp", strconv.Itoa(3 + len(files))}, argv...)
		files = append(files, filter)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	cmd, err := s.cfg.Launcher(runCtx, argv, files)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	// Registered after the fact because the launcher starts the process. Safe:
	// a child that exited in the gap has its status waiting in the reaper, and
	// Wait looks there first.
	if s.reaper != nil {
		s.reaper.Adopt(cmd.Process.Pid)
	}

	sockPath := path.Join(s.cfg.Layout.IPCDir, bwrap.InitSocketName)
	if err := waitForSocket(runCtx, sockPath, s.cfg.InitDial); err != nil {
		cancel()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("sandbox did not come up: %w", err)
	}

	sb := &sandbox{
		id:      req.SandboxID,
		tenant:  req.Tenant,
		token:   req.Token,
		cmd:     cmd,
		boundAt: time.Now(),
		argv:    argv,
		cancel:  cancel,
		reaper:  s.reaper,
	}
	if req.TTLSeconds > 0 {
		sb.expiresAt = sb.boundAt.Add(time.Duration(req.TTLSeconds) * time.Second)
	}
	return sb, nil
}

// waitForSocket polls until jdix-init is accepting connections. Polling beats a
// fixed sleep here: the whole point of the warm pool is that this takes tens of
// milliseconds, and a conservative sleep would give that saving straight back.
func waitForSocket(ctx context.Context, sockPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return lastErr
}

// stop tears the sandbox down: SIGTERM, a grace period, then SIGKILL. bwrap's
// --die-with-parent means the whole namespace goes with it.
func (sb *sandbox) stop(grace time.Duration) {
	if sb.ttlTimer != nil {
		sb.ttlTimer.Stop()
	}
	if sb.cmd != nil && sb.cmd.Process != nil {
		_ = sb.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		// The reaper owns wait4 where it is running, so waiting directly here
		// would race it for the status and one of the two would see ECHILD.
		go func() {
			if sb.reaper != nil {
				sb.reaper.Wait(sb.cmd.Process.Pid)
			} else {
				_, _ = sb.cmd.Process.Wait()
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(grace):
			_ = sb.cmd.Process.Kill()
		}
	}
	if sb.cancel != nil {
		sb.cancel()
	}
}

// unauthenticated errors are deliberately vague so the data plane cannot be
// used to probe whether a sandbox exists.
var errUnauthorized = &api.Error{Code: "unauthorized", Message: "invalid or missing sandbox token"}
