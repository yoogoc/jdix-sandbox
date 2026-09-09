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
)

// Launcher starts the sandbox process. Production runs bubblewrap; tests
// substitute a launcher that runs jdix-init directly, which exercises every
// line of the bind path that does not need a kernel namespace.
type Launcher func(ctx context.Context, argv []string) (*exec.Cmd, error)

// BwrapLauncher is the production launcher.
func BwrapLauncher(bwrapPath string) Launcher {
	return func(ctx context.Context, argv []string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, bwrapPath, argv...)
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
	ttlTimer  *time.Timer
}

// launch builds the argv, starts the sandbox process and waits for jdix-init to
// come up. It returns only once the namespace is usable, so a 200 from
// /internal/v1/bind genuinely means ready (DESIGN.md §07.4).
func (s *Server) launch(ctx context.Context, req api.BindRequest) (*sandbox, error) {
	policy := bwrap.DefaultPolicy(s.cfg.Tier)
	policy.UID, policy.GID = s.cfg.UID, s.cfg.GID
	policy.Volumes = s.cfg.Volumes

	argv, err := bwrap.Generate(req.Filesystem, policy, s.cfg.Layout)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	cmd, err := s.cfg.Launcher(runCtx, argv)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start sandbox: %w", err)
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
		go func() { _, _ = sb.cmd.Process.Wait(); close(done) }()
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
