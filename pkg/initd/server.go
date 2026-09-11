// Package initd is jdix-init: PID 1 inside the bubblewrap namespace.
//
// It is the real implementation of the sandbox data plane. execd, which lives
// outside the namespace, handles authentication and lifecycle and reverse
// proxies here over a unix socket. Putting exec and the file API inside means
// they see exactly the filesystem the tenant's spec described, and nothing else
// — there is no second, wider view for them to be tricked into using.
package initd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/reaper"
)

// Server holds the configuration handed over at bind time plus the live
// process table.
type Server struct {
	log       *slog.Logger
	startedAt time.Time

	mu        sync.RWMutex
	sandboxID string
	env       map[string]string
	roots     []api.Root
	workspace string
	ready     bool

	procs  *procTable
	reaper *reaper.Reaper
}

// New returns an unconfigured server. It answers /health immediately so execd
// can tell the namespace is alive before a sandbox is bound to it.
func New(log *slog.Logger, workspace string) *Server {
	return &Server{
		log:       log,
		startedAt: time.Now(),
		workspace: workspace,
		env:       map[string]string{},
		procs:     newProcTable(),
		reaper:    reaper.NewReaper(),
	}
}

// Reaper exposes the process reaper so main can run it for the lifetime of the
// process. Nothing else in the program may call wait4.
func (s *Server) Reaper() *reaper.Reaper { return s.reaper }

// Route is one entry of the data-plane surface.
//
// These are data rather than a sequence of mux calls so the OpenAPI document
// and the server can be compared: net/http's ServeMux does not expose what was
// registered, so without a table there is nothing to check the spec against.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// Routes is the public surface, the part gateway proxies and the SDKs call.
//
// /health and /configure are excluded: they are execd's private conversation
// with this process over a unix socket, not part of anyone's contract.
func (s *Server) Routes() []Route {
	return []Route{
		{"POST", "/v1/exec", s.handleExec},
		{"GET", "/v1/exec/stream", s.handleExecStream},
		{"GET", "/v1/pty", s.handlePTY},
		{"GET", "/v1/processes", s.handleProcessList},
		{"DELETE", "/v1/processes/{pid}", s.handleProcessSignal},

		{"GET", "/v1/files", s.handleFileGet},
		{"PUT", "/v1/files", s.handleFilePut},
		{"DELETE", "/v1/files", s.handleFileDelete},
		{"GET", "/v1/files/list", s.handleFileList},
	}
}

// Handler builds the mux. Every route here is unauthenticated on purpose: the
// only thing that can reach the socket is execd, and the socket is 0600.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /configure", s.handleConfigure)
	for _, r := range s.Routes() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	return mux
}

// maxUnixPath is the smallest sun_path any platform we target offers (macOS is
// 104, Linux 108). Exceeding it makes bind fail with a bare "invalid argument",
// which is a miserable thing to debug, so we say what is actually wrong.
const maxUnixPath = 104

// Listen creates the unix socket with permissions that keep it to execd.
func Listen(path string) (net.Listener, error) {
	if len(path) >= maxUnixPath {
		return nil, fmt.Errorf("socket path is %d bytes; the kernel limit is %d: %s",
			len(path), maxUnixPath-1, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A stale socket from a crashed predecessor would otherwise make bind fail.
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"ready":     s.ready,
		"sandboxId": s.sandboxID,
		"pid":       os.Getpid(),
		"uptimeMs":  time.Since(s.startedAt).Milliseconds(),
		"processes": s.procs.count(),
	})
}

// handleConfigure is the moment a bare namespace becomes a specific sandbox.
func (s *Server) handleConfigure(w http.ResponseWriter, r *http.Request) {
	var req api.ConfigureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	// Secrets are merged into the same environment the tenant sees but are kept
	// out of every log line and every API response.
	for k, v := range req.Secrets {
		env[k] = v
	}
	ws := req.Workspace
	if ws == "" {
		ws = s.workspace
	}
	roots := req.Roots
	if len(roots) == 0 {
		roots = []api.Root{{Path: ws}}
	}

	s.mu.Lock()
	if s.ready {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "already_configured", "initialization is one-shot")
		return
	}
	s.sandboxID, s.env, s.roots, s.workspace, s.ready = req.SandboxID, env, roots, ws, true
	s.mu.Unlock()

	s.log.Info("configured", "sandbox", req.SandboxID, "roots", len(roots), "envKeys", len(req.Env), "secretKeys", len(req.Secrets))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) snapshot() (env map[string]string, roots []api.Root, ws string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	env = make(map[string]string, len(s.env))
	for k, v := range s.env {
		env[k] = v
	}
	return env, append([]api.Root(nil), s.roots...), s.workspace
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, api.Error{Code: errCode, Message: msg})
}
