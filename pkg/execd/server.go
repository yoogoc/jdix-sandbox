package execd

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/isolation"
)

// Server holds the node measurement and, once bound, the live sandbox.
type Server struct {
	log       *slog.Logger
	cfg       Config
	startedAt time.Time
	probe     isolation.Result

	// internalToken authenticates the controller on the control plane. Empty
	// means "cluster-internal port, no token", which is only appropriate when a
	// NetworkPolicy already restricts who can reach :8081.
	internalToken string

	mu      sync.RWMutex
	sb      *sandbox
	expired bool

	// onExpire lets main terminate the process when the TTL fires, so the Pod
	// disappears rather than lingering as an empty shell.
	onExpire func(reason string)
}

func New(log *slog.Logger, cfg Config, probe isolation.Result, internalToken string) *Server {
	if cfg.InitDial == 0 {
		cfg.InitDial = 10 * time.Second
	}
	if cfg.GraceKill == 0 {
		cfg.GraceKill = 5 * time.Second
	}
	if cfg.UID == 0 {
		cfg.UID, cfg.GID = 1000, 1000
	}
	return &Server{log: log, cfg: cfg, startedAt: time.Now(), probe: probe, internalToken: internalToken}
}

// SetOnExpire registers the shutdown hook used when a TTL elapses.
func (s *Server) SetOnExpire(f func(reason string)) { s.onExpire = f }

// ControlHandler is the cluster-internal surface on :8081.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /internal/v1/bind", s.requireInternal(http.HandlerFunc(s.handleBind)))
	mux.Handle("POST /internal/v1/unbind", s.requireInternal(http.HandlerFunc(s.handleUnbind)))
	mux.Handle("GET /internal/v1/status", s.requireInternal(http.HandlerFunc(s.handleStatus)))
	// probe is deliberately unauthenticated: it is the kubelet's readiness
	// check, and the kubelet cannot carry our token.
	mux.HandleFunc("GET /internal/v1/probe", s.handleProbe)
	return mux
}

func (s *Server) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.internalToken != "" && !tokenMatches(bearer(r), s.internalToken) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid control-plane token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleProbe answers the readiness check. A node whose measured tier is below
// what the template demands must never join the warm pool, so this reports the
// tier and lets the controller decide (DESIGN.md §04.5).
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"isolationTier": s.probe.Tier,
		"reason":        s.probe.Reason,
		"probes":        s.probe.Probes,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resp := api.StatusResponse{
		IsolationTier: string(s.probe.Tier),
		TierReason:    s.probe.Reason,
		StartedAt:     s.startedAt,
	}
	if s.sb != nil {
		resp.Bound = true
		resp.SandboxID = s.sb.id
		if !s.sb.expiresAt.IsZero() {
			e := s.sb.expiresAt
			resp.ExpiresAt = &e
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	var req api.BindRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.SandboxID == "" || req.Token == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "sandboxId and token are required")
		return
	}

	s.mu.Lock()
	if s.sb != nil {
		id := s.sb.id
		s.mu.Unlock()
		// A Pod serves exactly one sandbox for its whole life. Re-binding would
		// mean a second tenant inheriting the first one's namespace.
		writeErr(w, http.StatusConflict, "already_bound", "this Pod is already bound to sandbox "+id)
		return
	}
	if s.expired {
		s.mu.Unlock()
		writeErr(w, http.StatusGone, "expired", "this Pod is shutting down")
		return
	}
	s.mu.Unlock()

	sb, err := s.launch(r.Context(), req)
	if err != nil {
		s.log.Error("bind failed", "sandbox", req.SandboxID, "err", err)
		writeErr(w, http.StatusBadRequest, "bind_failed", err.Error())
		return
	}

	// Configure runs before we publish the sandbox, so the data plane is never
	// reachable in a half-set-up state.
	if err := s.configureInit(r.Context(), req); err != nil {
		sb.stop(s.cfg.GraceKill)
		s.log.Error("configure failed", "sandbox", req.SandboxID, "err", err)
		writeErr(w, http.StatusInternalServerError, "configure_failed", err.Error())
		return
	}

	s.mu.Lock()
	s.sb = sb
	if !sb.expiresAt.IsZero() {
		sb.ttlTimer = time.AfterFunc(time.Until(sb.expiresAt), func() { s.expire("ttl") })
	}
	s.mu.Unlock()

	s.log.Info("bound", "sandbox", sb.id, "tenant", sb.tenant, "tier", s.probe.Tier,
		"mounts", len(req.Filesystem.Mounts), "expiresAt", sb.expiresAt)

	writeJSON(w, http.StatusOK, api.BindResponse{
		SandboxID:     sb.id,
		IsolationTier: string(s.probe.Tier),
		BoundAt:       sb.boundAt,
		ExpiresAt:     sb.expiresAt,
		BwrapArgs:     sb.argv,
	})
}

func (s *Server) handleUnbind(w http.ResponseWriter, r *http.Request) {
	s.expire("unbind")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// expire tears the sandbox down and marks the Pod unusable. Both the TTL and an
// explicit unbind land here, so there is one teardown path rather than two.
func (s *Server) expire(reason string) {
	s.mu.Lock()
	sb := s.sb
	if s.expired {
		s.mu.Unlock()
		return
	}
	s.expired = true
	s.sb = nil
	s.mu.Unlock()

	if sb != nil {
		s.log.Info("expiring sandbox", "sandbox", sb.id, "reason", reason)
		sb.stop(s.cfg.GraceKill)
	}
	if s.onExpire != nil {
		s.onExpire(reason)
	}
}

// authorised reports whether the request carries the bound sandbox's token.
func (s *Server) authorised(r *http.Request) (*sandbox, bool) {
	s.mu.RLock()
	sb := s.sb
	s.mu.RUnlock()
	if sb == nil {
		return nil, false
	}
	return sb, tokenMatches(bearer(r), sb.token)
}

// tokenMatches compares in constant time so the data plane cannot be used as an
// oracle to recover a token byte by byte.
func tokenMatches(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// Accepted as a convenience for browser contexts that cannot set headers,
	// such as a WebSocket opened directly from the Console.
	return r.URL.Query().Get("token")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, api.Error{Code: errCode, Message: msg})
}
