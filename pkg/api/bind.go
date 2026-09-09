package api

import "time"

// BindRequest is the body of POST /internal/v1/bind, sent by jdix-controller to
// execd once a Pod has been claimed for a Sandbox. It is the moment bwrap is
// built: the mount set only exists here, never earlier (DESIGN.md §04.1).
type BindRequest struct {
	SandboxID  string            `json:"sandboxId"`
	Tenant     string            `json:"tenant"`
	Filesystem FilesystemSpec    `json:"filesystem"`
	Env        map[string]string `json:"env,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"` // injected in memory only
	TTLSeconds int               `json:"ttlSeconds"`
	Token      string            `json:"token"` // one-shot data-plane bearer token
}

// BindResponse is returned only after the namespace is live and the data plane
// is accepting requests, so a 200 means "ready", not "accepted".
type BindResponse struct {
	SandboxID     string    `json:"sandboxId"`
	IsolationTier string    `json:"isolationTier"`
	BoundAt       time.Time `json:"boundAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	BwrapArgs     []string  `json:"bwrapArgs,omitempty"` // echoed for debugging/audit
}

// StatusResponse is served by GET /internal/v1/status.
type StatusResponse struct {
	IsolationTier string     `json:"isolationTier"`
	TierReason    string     `json:"tierReason"`
	Bound         bool       `json:"bound"`
	SandboxID     string     `json:"sandboxId,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
}

// Error is the uniform error body for every HTTP surface.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
