package jdix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NoExpiry asks for a sandbox that never expires, for CreateOpts.TTL.
//
// The template has to permit it by setting no maxTTLSeconds; against a template
// that caps lifetimes this yields the cap. Nothing then reclaims the sandbox if
// the caller goes away, so close it yourself — a deferred Close, or a Delete
// from whatever outlives the process.
const NoExpiry = time.Duration(-1)

// CreateOpts describes the sandbox you want.
type CreateOpts struct {
	Template string
	// TTL is how long the sandbox may live. Zero takes the template's default;
	// NoExpiry asks for one that never expires.
	TTL      time.Duration
	Env      map[string]string
	Mounts   []Mount
	Metadata map[string]string

	// IdempotencyKey makes a retried create return the first sandbox instead of
	// starting a second one. Set it whenever a create might be replayed — a job
	// runner, a queue consumer, anything with an at-least-once delivery model.
	IdempotencyKey string

	// KeepAlive extends the TTL in the background for as long as the Sandbox is
	// open. Convenient for interactive work; leave it off for batch jobs, where
	// a hung process should be allowed to hit its deadline.
	KeepAlive bool
}

// Mount binds part of a template-declared volume into the sandbox.
type Mount struct {
	Path     string `json:"path"`
	Volume   string `json:"volume"`
	SubPath  string `json:"subPath,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type createRequest struct {
	Template   string            `json:"template"`
	TTLSeconds int               `json:"ttlSeconds,omitempty"`
	Filesystem *filesystemSpec   `json:"filesystem,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type filesystemSpec struct {
	Mounts []mountSpec `json:"mounts,omitempty"`
}

type mountSpec struct {
	Path     string `json:"path"`
	Source   source `json:"source"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type source struct {
	Volume  string `json:"volume"`
	SubPath string `json:"subPath,omitempty"`
}

type sandboxJSON struct {
	ID            string     `json:"id"`
	State         string     `json:"state"`
	Template      string     `json:"template"`
	IsolationTier string     `json:"isolationTier"`
	Endpoint      string     `json:"endpoint"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	ColdStart     bool       `json:"coldStart"`
	Reason        string     `json:"reason"`
}

// Sandbox is a live execution environment.
//
// It is safe for concurrent use: several goroutines may run commands or move
// files at once.
type Sandbox struct {
	ID            string
	Template      string
	IsolationTier string
	Endpoint      string
	ColdStart     bool

	// Files is the file API, scoped to the sandbox's workspace and mounts.
	Files *Files

	client *Client

	mu        sync.RWMutex
	state     string
	expiresAt *time.Time

	keepAliveStop chan struct{}
	closeOnce     sync.Once
}

// Create starts a sandbox and waits for it to be usable.
//
// It returns as soon as the control plane says the sandbox is running. On a
// warm-pool hit that is a couple of hundred milliseconds; on a cold start the
// call may return with the sandbox still pending, and WaitReady finishes the
// job — so a caller who cannot proceed without it should call that.
func (c *Client) Create(ctx context.Context, opts CreateOpts) (*Sandbox, error) {
	if opts.Template == "" {
		return nil, errors.New("jdix: CreateOpts.Template is required")
	}
	req := createRequest{
		Template: opts.Template,
		Env:      opts.Env,
		Metadata: opts.Metadata,
	}
	switch {
	case opts.TTL == NoExpiry:
		req.TTLSeconds = -1
	case opts.TTL > 0:
		req.TTLSeconds = int(opts.TTL.Seconds())
	}
	if len(opts.Mounts) > 0 {
		fs := &filesystemSpec{}
		for _, m := range opts.Mounts {
			fs.Mounts = append(fs.Mounts, mountSpec{
				Path:     m.Path,
				Source:   source{Volume: m.Volume, SubPath: m.SubPath},
				ReadOnly: m.ReadOnly,
			})
		}
		req.Filesystem = fs
	}

	headers := map[string]string{}
	if opts.IdempotencyKey != "" {
		headers["Idempotency-Key"] = opts.IdempotencyKey
	}

	var out sandboxJSON
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", req, &out, headers); err != nil {
		return nil, err
	}
	if out.State == "failed" {
		return nil, &Error{StatusCode: http.StatusUnprocessableEntity, Code: "sandbox_failed", Message: out.Reason}
	}

	sbx := c.newSandbox(&out)
	// A sandbox that never expires has no deadline to renew, so asking for both
	// is not an error, it is just nothing to do.
	if opts.KeepAlive && opts.TTL != NoExpiry {
		sbx.startKeepAlive(opts.TTL)
	}
	return sbx, nil
}

func (c *Client) newSandbox(j *sandboxJSON) *Sandbox {
	s := &Sandbox{
		ID:            j.ID,
		Template:      j.Template,
		IsolationTier: j.IsolationTier,
		Endpoint:      j.Endpoint,
		ColdStart:     j.ColdStart,
		client:        c,
		state:         j.State,
		expiresAt:     j.ExpiresAt,
	}
	s.Files = &Files{sbx: s}
	return s
}

// Get fetches an existing sandbox, ready to execute against.
//
// The data-plane token comes back with it, so a process that lost the one from
// Create — a restart, a different worker picking up the job — can reattach by
// id alone. List does not carry tokens; use Get for the ones you mean to use.
func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	var out sandboxJSON
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &out, nil); err != nil {
		return nil, err
	}
	return c.newSandbox(&out), nil
}

// List returns this tenant's sandboxes, optionally filtered by state.
func (c *Client) List(ctx context.Context, state string) ([]*Sandbox, error) {
	path := "/v1/sandboxes"
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	var out struct {
		Sandboxes []sandboxJSON `json:"sandboxes"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out, nil); err != nil {
		return nil, err
	}
	res := make([]*Sandbox, 0, len(out.Sandboxes))
	for i := range out.Sandboxes {
		res = append(res, c.newSandbox(&out.Sandboxes[i]))
	}
	return res, nil
}

// Template describes a template the caller may use.
type Template struct {
	Name            string `json:"name"`
	Admission       string `json:"admission"`
	AdmissionReason string `json:"admissionReason"`
	MinTier         string `json:"minIsolationTier"`
	DefaultTTL      int    `json:"defaultTTLSeconds"`
	MaxTTL          int    `json:"maxTTLSeconds"`
	HasVolumes      bool   `json:"hasVolumes"`
}

// Templates lists what this key is allowed to run.
func (c *Client) Templates(ctx context.Context) ([]Template, error) {
	var out struct {
		Templates []Template `json:"templates"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/templates", nil, &out, nil); err != nil {
		return nil, err
	}
	return out.Templates, nil
}

// State reports the last known phase.
func (s *Sandbox) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// ExpiresAt reports the current deadline, or nil if there is none.
func (s *Sandbox) ExpiresAt() *time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}

// WaitReady polls until the sandbox is running or the context ends.
//
// Only cold starts need it: a warm-pool hit is already running by the time
// Create returns.
func (s *Sandbox) WaitReady(ctx context.Context) error {
	if s.State() == "running" {
		return nil
	}
	// Start tight and ease off: most waits end almost immediately, and the few
	// that do not are waiting on an image pull measured in seconds.
	delay := 100 * time.Millisecond
	for {
		fresh, err := s.client.Get(ctx, s.ID)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.state, s.expiresAt, s.Endpoint = fresh.state, fresh.expiresAt, fresh.Endpoint
		s.IsolationTier = fresh.IsolationTier
		state := s.state
		s.mu.Unlock()

		switch state {
		case "running":
			return nil
		case "failed":
			return &Error{StatusCode: http.StatusUnprocessableEntity, Code: "sandbox_failed", Message: fresh.state}
		case "expired":
			return ErrExpired
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// KeepAlive extends the deadline once.
func (s *Sandbox) KeepAlive(ctx context.Context, ttl time.Duration) error {
	secs := int(ttl.Seconds())
	if secs <= 0 {
		secs = 300
	}
	var out struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := s.dataPlane(ctx, http.MethodPost,
		"/v1/keepalive?ttlSeconds="+strconv.Itoa(secs), nil, &out); err != nil {
		return err
	}
	s.mu.Lock()
	s.expiresAt = out.ExpiresAt
	s.mu.Unlock()
	return nil
}

// startKeepAlive renews in the background at a third of the TTL, so a single
// missed renewal is not fatal.
func (s *Sandbox) startKeepAlive(ttl time.Duration) {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	interval := ttl / 3
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	s.keepAliveStop = make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.keepAliveStop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = s.KeepAlive(ctx, ttl)
				cancel()
			}
		}
	}()
}

// Close deletes the sandbox. It is safe to call more than once, so
// `defer sbx.Close(ctx)` next to a later explicit Close is not a bug.
func (s *Sandbox) Close(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		if s.keepAliveStop != nil {
			close(s.keepAliveStop)
		}
		err = s.client.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(s.ID), nil, nil, nil)
		if e := (*Error)(nil); errors.As(err, &e) && e.StatusCode == http.StatusNotFound {
			// Already gone, most likely because the TTL elapsed. That is the
			// outcome Close wanted.
			err = nil
		}
	})
	return err
}

// endpointURL builds a data-plane URL for this sandbox.
func (s *Sandbox) endpointURL(path string) (string, error) {
	if s.Endpoint == "" {
		return "", fmt.Errorf("jdix: sandbox %s has no endpoint yet; call WaitReady first", s.ID)
	}
	return strings.TrimSuffix(s.Endpoint, "/") + path, nil
}
