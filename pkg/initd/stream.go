package initd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"jdix.io/sandbox/pkg/api"
)

// handleExecStream runs a command with stdin/stdout/stderr carried over one
// WebSocket, using the shared frame protocol.
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	req, err := execReqFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	conn, err := s.acceptWS(w, r)
	if err != nil {
		return // Accept already answered the request
	}
	defer conn.c.CloseNow()

	ctx, cancel := context.WithTimeout(r.Context(), timeoutOf(req))
	defer cancel()

	cmd, label, err := s.buildCmd(ctx, req)
	if err != nil {
		_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "invalid_request", Msg: err.Error()})
		return
	}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	start := time.Now()
	pid, err := s.start(ctx, cmd, label)
	if err != nil {
		_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "spawn_failed", Msg: err.Error()})
		return
	}
	defer s.procs.forget(pid)

	go keepalive(ctx, conn)
	done := make(chan struct{}, 2)
	go func() { pumpStream(ctx, conn, api.FrameStdout, stdout); done <- struct{}{} }()
	go func() { pumpStream(ctx, conn, api.FrameStderr, stderr); done <- struct{}{} }()
	go s.readControl(ctx, conn, pid, stdin, nil)

	code := s.reaper.Wait(pid)
	s.procs.finish(pid)
	<-done
	<-done

	_ = conn.send(ctx, api.Frame{
		T: api.FrameExit, Code: code, DurationMs: time.Since(start).Milliseconds(),
	})
	_ = conn.c.Close(websocket.StatusNormalClosure, "")
}

// handlePTY is the same idea with a real terminal, so interactive programs and
// the Console's web terminal behave the way a shell expects.
func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	req, err := execReqFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Cmd == "" && len(req.Argv) == 0 {
		req.Argv = []string{"/bin/sh", "-l"}
	}
	conn, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	defer conn.c.CloseNow()

	ctx, cancel := context.WithTimeout(r.Context(), timeoutOf(req))
	defer cancel()

	cmd, label, err := s.buildCmd(ctx, req)
	if err != nil {
		_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "invalid_request", Msg: err.Error()})
		return
	}
	// A PTY session needs its own session and controlling terminal; Setpgid
	// alone is not enough for job control to work inside the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	cols, rows := sizeFromQuery(r)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "pty_failed", Msg: err.Error()})
		return
	}
	defer f.Close()

	pid := cmd.Process.Pid
	s.reaper.Adopt(pid)
	s.procs.addPID(pid, cmd, label)
	defer s.procs.forget(pid)
	go s.enforceDeadline(ctx, pid)

	start := time.Now()
	go keepalive(ctx, conn)
	done := make(chan struct{})
	// A PTY merges stdout and stderr by design; reporting both as "stdout"
	// keeps the frame protocol honest rather than inventing a split.
	go func() { pumpStream(ctx, conn, api.FrameStdout, f); close(done) }()
	go s.readControl(ctx, conn, pid, f, f)

	code := s.reaper.Wait(pid)
	s.procs.finish(pid)
	_ = f.Close()
	<-done

	_ = conn.send(ctx, api.Frame{
		T: api.FrameExit, Code: code, DurationMs: time.Since(start).Milliseconds(),
	})
	_ = conn.c.Close(websocket.StatusNormalClosure, "")
}

// readControl handles the client-to-server half: stdin, resize, signals, ping.
// resizeTarget is nil for non-PTY sessions, where a resize frame is meaningless.
func (s *Server) readControl(ctx context.Context, conn *wsConn, pid int, stdin io.WriteCloser, resizeTarget *os.File) {
	for {
		_, data, err := conn.c.Read(ctx)
		if err != nil {
			if stdin != nil {
				_ = stdin.Close() // EOF to the child, so it can finish
			}
			return
		}
		var f api.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "bad_frame", Msg: err.Error()})
			continue
		}
		switch f.T {
		case api.FrameStdin:
			raw, err := base64.StdEncoding.DecodeString(f.D)
			if err != nil {
				_ = conn.send(ctx, api.Frame{T: api.FrameError, ErrCode: "bad_base64", Msg: err.Error()})
				continue
			}
			if stdin != nil {
				if _, err := stdin.Write(raw); err != nil {
					return
				}
			}
		case api.FrameResize:
			if resizeTarget != nil && f.Cols > 0 && f.Rows > 0 {
				_ = pty.Setsize(resizeTarget, &pty.Winsize{Cols: uint16(f.Cols), Rows: uint16(f.Rows)})
			}
		case api.FrameSignal:
			sig := signalByName(f.Sig)
			if err := syscall.Kill(-pid, sig); err != nil {
				_ = syscall.Kill(pid, sig)
			}
		case api.FramePing:
			_ = conn.send(ctx, api.Frame{T: api.FramePong})
		case api.FramePong:
			// client answering our keepalive; nothing to do
		}
	}
}

func execReqFromQuery(r *http.Request) (api.ExecRequest, error) {
	q := r.URL.Query()
	req := api.ExecRequest{
		Cmd: q.Get("cmd"),
		Cwd: q.Get("cwd"),
	}
	if v := q.Get("timeoutSeconds"); v != "" {
		req.TimeoutSeconds, _ = strconv.Atoi(v)
	}
	if argv := q["argv"]; len(argv) > 0 {
		req.Argv = argv
	}
	return req, nil
}

func sizeFromQuery(r *http.Request) (cols, rows int) {
	cols, rows = 120, 40
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && v > 0 {
		cols = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && v > 0 {
		rows = v
	}
	return cols, rows
}
