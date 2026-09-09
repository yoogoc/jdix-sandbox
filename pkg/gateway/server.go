package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"jdix.io/sandbox/pkg/api"
	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

// Target is where a sandbox currently lives.
type Target struct {
	SandboxID string
	PodIP     string
	ExpiresAt time.Time
	Phase     string
}

// Resolver maps a sandbox id to its Pod.
type Resolver interface {
	Resolve(ctx context.Context, id string) (Target, bool)
}

// CachedResolver reads from an informer cache and keeps a small local map in
// front of it.
//
// It is keyed by sandbox id rather than by Pod IP because Kubernetes reuses IPs
// promptly: an entry keyed by address could quietly start pointing at a
// different tenant's Pod.
type CachedResolver struct {
	Reader client.Reader
	TTL    time.Duration

	mu    sync.RWMutex
	cache map[string]cached
}

type cached struct {
	target Target
	at     time.Time
}

func NewCachedResolver(r client.Reader) *CachedResolver {
	return &CachedResolver{Reader: r, TTL: 2 * time.Second, cache: map[string]cached{}}
}

func (c *CachedResolver) Resolve(ctx context.Context, id string) (Target, bool) {
	c.mu.RLock()
	e, ok := c.cache[id]
	c.mu.RUnlock()
	if ok && time.Since(e.at) < c.TTL {
		return e.target, e.target.PodIP != ""
	}

	var list sbxv1.SandboxList
	if err := c.Reader.List(ctx, &list, client.MatchingFields{"metadata.name": id}); err != nil {
		return Target{}, false
	}
	if len(list.Items) == 0 {
		// Negative results are cached too, or a burst of requests for a deleted
		// sandbox turns into a burst of lookups.
		c.store(id, Target{})
		return Target{}, false
	}
	s := list.Items[0]
	t := Target{SandboxID: id, PodIP: s.Status.PodIP, Phase: string(s.Status.Phase)}
	if s.Status.ExpiresAt != nil {
		t.ExpiresAt = s.Status.ExpiresAt.Time
	}
	c.store(id, t)
	return t, t.PodIP != ""
}

func (c *CachedResolver) store(id string, t Target) {
	c.mu.Lock()
	c.cache[id] = cached{target: t, at: time.Now()}
	c.mu.Unlock()
}

// Server is the public entry point for sandbox traffic.
type Server struct {
	Resolver Resolver
	Suffix   string
	Log      *slog.Logger

	proxy *httputil.ReverseProxy
	once  sync.Once
}

func (s *Server) init() {
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := pr.In.Context().Value(targetKey{}).(*url.URL)
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// The gateway is the last hop that knows the real client, and the
			// sandbox is untrusted, so forwarding headers are set rather than
			// appended: a value the client supplied must not survive.
			if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set("X-Forwarded-For", ip)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		// Stream rather than buffer: exec output and PTY sessions are useless
		// if they arrive in blocks.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.Log.Warn("proxy error", "host", r.Host, "path", r.URL.Path, "err", err)
			writeErr(w, http.StatusBadGateway, "sandbox_unreachable", "the sandbox is not responding")
		},
	}
}

type targetKey struct{}

func (s *Server) Handler() http.Handler {
	s.once.Do(s.init)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/", s.route)
	return mux
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	exists := func(id string) bool {
		_, ok := s.Resolver.Resolve(r.Context(), id)
		return ok
	}
	rt, err := ParseHost(r.Host, s.Suffix, exists)
	if err != nil {
		writeErr(w, http.StatusNotFound, "unknown_sandbox",
			"this hostname does not correspond to a running sandbox")
		return
	}

	target, ok := s.Resolver.Resolve(r.Context(), rt.SandboxID)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_sandbox",
			"this hostname does not correspond to a running sandbox")
		return
	}
	if !target.ExpiresAt.IsZero() && time.Now().After(target.ExpiresAt) {
		writeErr(w, http.StatusGone, "expired", "this sandbox has expired")
		return
	}

	// The token is checked here and again by execd. Two checks, because a
	// misconfigured NetworkPolicy that let something reach a Pod directly must
	// not be the only thing standing between a stranger and a shell.
	if bearer(r) == "" {
		writeJSON(w, http.StatusUnauthorized,
			api.Error{Code: "unauthorized", Message: "a sandbox token is required"})
		return
	}

	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(target.PodIP, strconv.Itoa(rt.Port))}
	ctx := context.WithValue(r.Context(), targetKey{}, u)
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	// WebSockets opened from a browser cannot set headers, so the token may
	// also arrive in the query string.
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
