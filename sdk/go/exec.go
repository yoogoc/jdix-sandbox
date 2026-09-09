package jdix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Result is what a finished command produced.
//
// A non-zero ExitCode is not an error: the request succeeded and the command
// failed, and those are different things. Errors are reserved for the call not
// happening at all.
type Result struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timedOut"`
}

// Ok reports whether the command exited zero.
func (r Result) Ok() bool { return r.ExitCode == 0 }

// RunOpts tunes a single command.
type RunOpts struct {
	Cwd            string
	Env            map[string]string
	Timeout        time.Duration
	MaxOutputBytes int
	// Argv runs a program directly, with no shell. Prefer it whenever any part
	// of the command comes from untrusted input: there is nothing for a quoting
	// mistake to escape into.
	Argv []string
}

type execRequest struct {
	Cmd            string            `json:"cmd,omitempty"`
	Argv           []string          `json:"argv,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int               `json:"maxOutputBytes,omitempty"`
}

// Run executes a shell command and waits for it to finish.
func (s *Sandbox) Run(ctx context.Context, cmd string, opts ...RunOpts) (Result, error) {
	var o RunOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	req := execRequest{
		Cmd: cmd, Argv: o.Argv, Cwd: o.Cwd, Env: o.Env,
		MaxOutputBytes: o.MaxOutputBytes,
	}
	if o.Timeout > 0 {
		req.TimeoutSeconds = int(o.Timeout.Seconds())
	}
	var res Result
	if err := s.dataPlane(ctx, http.MethodPost, "/v1/exec", req, &res); err != nil {
		return Result{}, err
	}
	return res, nil
}

// Event is one message from a streaming session.
type Event struct {
	Kind       string // "stdout", "stderr", "exit", "error"
	Data       []byte
	ExitCode   int
	DurationMs int64
	Err        error
}

// Stream is a running command with live output.
type Stream struct {
	conn   *websocket.Conn
	events chan Event
	cancel context.CancelFunc
	ctx    context.Context
}

// Events yields output until the command exits or the connection drops. The
// channel is closed when there is nothing more to come.
func (st *Stream) Events() <-chan Event { return st.events }

// Stdin writes to the command's standard input.
func (st *Stream) Stdin(p []byte) error {
	return st.send(frame{T: "stdin", D: base64.StdEncoding.EncodeToString(p)})
}

// CloseStdin signals end of input, which is what most filters wait for.
func (st *Stream) CloseStdin() error { return st.send(frame{T: "stdin", D: ""}) }

// Signal sends a signal to the command's process group.
func (st *Stream) Signal(sig string) error { return st.send(frame{T: "signal", Sig: sig}) }

// Resize updates the terminal size. It only has an effect on a PTY session.
func (st *Stream) Resize(cols, rows int) error {
	return st.send(frame{T: "resize", Cols: cols, Rows: rows})
}

// Close ends the session.
func (st *Stream) Close() error {
	st.cancel()
	return st.conn.Close(websocket.StatusNormalClosure, "")
}

func (st *Stream) send(f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return st.conn.Write(st.ctx, websocket.MessageText, b)
}

type frame struct {
	T          string `json:"t"`
	D          string `json:"d,omitempty"`
	Seq        uint64 `json:"seq,omitempty"`
	Cols       int    `json:"cols,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	Sig        string `json:"sig,omitempty"`
	Code       int    `json:"code,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	ErrCode    string `json:"errCode,omitempty"`
	Msg        string `json:"msg,omitempty"`
}

// RunStream starts a command and streams its output.
func (s *Sandbox) RunStream(ctx context.Context, cmd string, opts ...RunOpts) (*Stream, error) {
	q := url.Values{"cmd": {cmd}}
	if len(opts) > 0 && opts[0].Timeout > 0 {
		q.Set("timeoutSeconds", strconv.Itoa(int(opts[0].Timeout.Seconds())))
	}
	return s.openStream(ctx, "/v1/exec/stream", q)
}

// PTYOpts configures an interactive session.
type PTYOpts struct {
	Cmd  string
	Cols int
	Rows int
}

// PTY opens an interactive shell with a real terminal, which is what programs
// that draw progress bars or prompt for input expect to find.
func (s *Sandbox) PTY(ctx context.Context, opts ...PTYOpts) (*Stream, error) {
	var o PTYOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	q := url.Values{}
	if o.Cmd != "" {
		q.Set("cmd", o.Cmd)
	}
	if o.Cols > 0 {
		q.Set("cols", strconv.Itoa(o.Cols))
	}
	if o.Rows > 0 {
		q.Set("rows", strconv.Itoa(o.Rows))
	}
	return s.openStream(ctx, "/v1/pty", q)
}

func (s *Sandbox) openStream(ctx context.Context, path string, q url.Values) (*Stream, error) {
	base, err := s.endpointURL(path)
	if err != nil {
		return nil, err
	}
	wsURL := strings.Replace(base, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	if len(q) > 0 {
		wsURL += "?" + q.Encode()
	}

	streamCtx, cancel := context.WithCancel(ctx)
	conn, _, err := websocket.Dial(streamCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + s.token}},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	// Output can be large; the default limit would cut a build log short.
	conn.SetReadLimit(32 << 20)

	st := &Stream{conn: conn, events: make(chan Event, 64), cancel: cancel, ctx: streamCtx}
	go st.pump()
	return st, nil
}

func (st *Stream) pump() {
	defer close(st.events)
	defer st.cancel()
	for {
		_, data, err := st.conn.Read(st.ctx)
		if err != nil {
			// A closed connection after the exit frame is the normal ending, so
			// it is not reported as a failure.
			if st.ctx.Err() == nil {
				st.events <- Event{Kind: "error", Err: err}
			}
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			st.events <- Event{Kind: "error", Err: err}
			continue
		}
		switch f.T {
		case "stdout", "stderr":
			raw, err := base64.StdEncoding.DecodeString(f.D)
			if err != nil {
				st.events <- Event{Kind: "error", Err: err}
				continue
			}
			st.events <- Event{Kind: f.T, Data: raw}
		case "exit":
			st.events <- Event{Kind: "exit", ExitCode: f.Code, DurationMs: f.DurationMs}
			return
		case "error":
			st.events <- Event{Kind: "error", Err: &Error{Code: f.ErrCode, Message: f.Msg}}
		case "ping":
			_ = st.send(frame{T: "pong"})
		}
	}
}

// Collect drains a stream into a Result, for when streaming was only needed to
// avoid buffering on the server side.
func (st *Stream) Collect() (Result, error) {
	var out, errBuf bytes.Buffer
	res := Result{ExitCode: -1}
	for ev := range st.Events() {
		switch ev.Kind {
		case "stdout":
			out.Write(ev.Data)
		case "stderr":
			errBuf.Write(ev.Data)
		case "exit":
			res.ExitCode, res.DurationMs = ev.ExitCode, ev.DurationMs
		case "error":
			return res, ev.Err
		}
	}
	res.Stdout, res.Stderr = out.String(), errBuf.String()
	return res, nil
}

// dataPlane performs a JSON request against the sandbox itself.
func (s *Sandbox) dataPlane(ctx context.Context, method, path string, body, out any) error {
	u, err := s.endpointURL(path)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("User-Agent", s.client.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if apiErr := decodeError(resp); apiErr != nil {
		return apiErr
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
