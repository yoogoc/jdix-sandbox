package api

// ExecRequest runs a command inside the sandbox. Cmd is passed to /bin/sh -c
// unless Argv is set, in which case it is executed directly with no shell.
type ExecRequest struct {
	Cmd            string            `json:"cmd,omitempty"`
	Argv           []string          `json:"argv,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int               `json:"maxOutputBytes,omitempty"`
}

// ExecResult is the synchronous form. Truncated says the output hit
// MaxOutputBytes, so a caller never mistakes a clipped log for a short one.
type ExecResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

// Process is one entry of GET /v1/processes.
type Process struct {
	PID     int    `json:"pid"`
	Cmd     string `json:"cmd"`
	Started string `json:"started"`
	Running bool   `json:"running"`
}

// FileInfo is one entry of GET /v1/files/list.
type FileInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	IsDir    bool   `json:"isDir"`
	Modified string `json:"modified"`
	Symlink  bool   `json:"symlink"`
}

// ConfigureRequest is execd -> jdix-init at bind time. Secrets travel here, over
// a unix socket, rather than through argv or the Pod spec (DESIGN.md §12 P1).
type ConfigureRequest struct {
	SandboxID string            `json:"sandboxId"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	Roots     []Root            `json:"roots"`
	Workspace string            `json:"workspace"`
}

// Root is a directory the file API is allowed to touch. Anything outside every
// root is a 403, whatever the path looks like.
type Root struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly"`
}
