package initd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"jdix.io/sandbox/pkg/api"
)

const (
	// defaultTimeout stops a forgotten command from occupying the sandbox for
	// its whole TTL.
	defaultTimeout = 120 * time.Second
	maxTimeout     = 6 * time.Hour
	// defaultMaxOutput bounds the synchronous form. Streaming has no such cap:
	// the client is reading as it goes.
	defaultMaxOutput = 4 << 20
	// gracePeriod matches the platform's graceful-destroy contract: SIGTERM,
	// wait, SIGKILL (DESIGN.md §12 P0).
	gracePeriod = 5 * time.Second
)

func (s *Server) buildCmd(ctx context.Context, req api.ExecRequest) (*exec.Cmd, string, error) {
	env, _, ws := s.snapshot()

	var cmd *exec.Cmd
	var label string
	switch {
	case len(req.Argv) > 0:
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
		label = req.Argv[0]
	case req.Cmd != "":
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", req.Cmd)
		label = req.Cmd
	default:
		return nil, "", errors.New("either cmd or argv is required")
	}

	cmd.Dir = ws
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	for k, v := range req.Env { // per-call env wins over the sandbox default
		env[k] = v
	}
	flat := make([]string, 0, len(env))
	for k, v := range env {
		flat = append(flat, k+"="+v)
	}
	cmd.Env = flat
	// Own process group, so a timeout or a signal reaches the whole tree rather
	// than just the shell that spawned it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, label, nil
}

func timeoutOf(req api.ExecRequest) time.Duration {
	if req.TimeoutSeconds <= 0 {
		return defaultTimeout
	}
	d := time.Duration(req.TimeoutSeconds) * time.Second
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}

// capWriter collects output up to a limit and then keeps counting without
// keeping bytes, so a runaway command cannot exhaust memory.
type capWriter struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
			c.over = true
		}
	} else if len(p) > 0 {
		c.over = true
	}
	return len(p), nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	limit := req.MaxOutputBytes
	if limit <= 0 {
		limit = defaultMaxOutput
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeoutOf(req))
	defer cancel()

	cmd, label, err := s.buildCmd(ctx, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pipe_failed", err.Error())
		return
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pipe_failed", err.Error())
		return
	}

	start := time.Now()
	pid, err := s.start(ctx, cmd, label)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "spawn_failed", err.Error())
		return
	}
	defer s.procs.forget(pid)

	stdout := &capWriter{limit: limit}
	stderr := &capWriter{limit: limit}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(stdout, outPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(stderr, errPipe) }()

	code := s.reaper.Wait(pid)
	s.procs.finish(pid)
	wg.Wait() // drain whatever the child wrote just before exiting

	res := api.ExecResult{
		ExitCode:   code,
		Stdout:     stdout.buf.String(),
		Stderr:     stderr.buf.String(),
		DurationMs: time.Since(start).Milliseconds(),
		Truncated:  stdout.over || stderr.over,
		TimedOut:   ctx.Err() == context.DeadlineExceeded,
	}
	writeJSON(w, http.StatusOK, res)
}

// start launches a command through the reaper and arms the timeout.
//
// exec.CommandContext would normally kill the process when the context expires,
// but it does that from inside cmd.Wait(), which we never call — the reaper owns
// waiting. So the deadline is enforced here instead, against the process group
// so a shell's children die with it.
func (s *Server) start(ctx context.Context, cmd *exec.Cmd, label string) (int, error) {
	pid, err := s.reaper.StartCmd(cmd)
	if err != nil {
		return 0, err
	}
	s.procs.addPID(pid, cmd, label)
	go s.enforceDeadline(ctx, pid)
	return pid, nil
}

// enforceDeadline terminates a process group once its context expires: SIGTERM,
// a grace period, then SIGKILL. Signalling the negative pid targets the group,
// so a shell's children go with it instead of being orphaned.
func (s *Server) enforceDeadline(ctx context.Context, pid int) {
	<-ctx.Done()
	if done, ok := s.procs.status(pid); ok && done {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(gracePeriod)
	if done, ok := s.procs.status(pid); ok && !done {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func (s *Server) handleProcessList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"processes": s.procs.list()})
}

func (s *Server) handleProcessSignal(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_pid", err.Error())
		return
	}
	proc, ok := s.procs.process(pid)
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_process", "this sandbox did not start that process")
		return
	}
	sig := signalByName(r.URL.Query().Get("signal"))
	// Negative pid targets the group, so children of the shell die too.
	if err := syscall.Kill(-pid, sig); err != nil {
		if err := proc.Signal(sig); err != nil {
			writeErr(w, http.StatusInternalServerError, "signal_failed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": pid, "signal": sig.String()})
}

func signalByName(name string) syscall.Signal {
	switch name {
	case "SIGINT", "INT", "2":
		return syscall.SIGINT
	case "SIGQUIT", "QUIT", "3":
		return syscall.SIGQUIT
	case "SIGKILL", "KILL", "9":
		return syscall.SIGKILL
	case "SIGHUP", "HUP", "1":
		return syscall.SIGHUP
	default:
		return syscall.SIGTERM
	}
}

// ── streaming ────────────────────────────────────────────────────────────

// wsConn serialises writes: a WebSocket connection may not be written from two
// goroutines at once, and stdout, stderr and pong all want to.
type wsConn struct {
	c   *websocket.Conn
	mu  sync.Mutex
	seq atomic.Uint64
}

func (w *wsConn) send(ctx context.Context, f api.Frame) error {
	if f.T == api.FrameStdout || f.T == api.FrameStderr || f.T == api.FrameEvent {
		f.Seq = w.seq.Add(1)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageText, b)
}

func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// execd is the only origin that can reach this socket, and it has
		// already authenticated the caller.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(8 << 20)
	return &wsConn{c: c}, nil
}

// pumpStream copies a reader into frames of the given type.
func pumpStream(ctx context.Context, conn *wsConn, kind string, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := conn.send(ctx, api.Frame{
				T: kind, D: base64.StdEncoding.EncodeToString(buf[:n]),
			}); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// keepalive answers the design's application-level ping requirement: a load
// balancer that drops WebSocket control frames still sees traffic.
func keepalive(ctx context.Context, conn *wsConn) {
	t := time.NewTicker(api.PingIntervalSeconds * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.send(ctx, api.Frame{T: api.FramePing}); err != nil {
				return
			}
		}
	}
}
