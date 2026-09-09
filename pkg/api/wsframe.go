package api

// One frame shape serves exec streaming, PTY sessions and file watches, so the
// SDKs and the Console terminal share a single codec (DESIGN.md §07.2).
//
// Payloads are base64 rather than binary WebSocket frames: intermediaries
// handle text frames far more predictably, and the size cost is irrelevant next
// to a shell session.
type Frame struct {
	T          string `json:"t"`
	D          string `json:"d,omitempty"`    // base64 payload
	Seq        uint64 `json:"seq,omitempty"`  // server->client, monotonic, for resume
	Cols       int    `json:"cols,omitempty"` // resize
	Rows       int    `json:"rows,omitempty"` // resize
	Sig        string `json:"sig,omitempty"`  // signal
	Code       int    `json:"code,omitempty"` // exit status
	DurationMs int64  `json:"durationMs,omitempty"`
	ErrCode    string `json:"errCode,omitempty"`
	Msg        string `json:"msg,omitempty"`
}

// Frame types.
const (
	FrameStdin  = "stdin"
	FrameStdout = "stdout"
	FrameStderr = "stderr"
	FrameResize = "resize"
	FrameSignal = "signal"
	FrameExit   = "exit"
	FrameError  = "error"
	FramePing   = "ping"
	FramePong   = "pong"
	FrameEvent  = "event" // file watch
)

// PingInterval is the application-level keepalive. WebSocket control frames are
// not reliably forwarded by every load balancer in the path, which is the usual
// cause of a session that dies silently after a minute of quiet.
const PingIntervalSeconds = 30
