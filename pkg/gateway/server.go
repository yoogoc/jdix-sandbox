package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"jdix.io/sandbox/pkg/api"
	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

// Target is where a sandbox currently lives.
//
// Both a Pod IP and a Pod name are carried because the two transports need
// different ones: dialling wants the address, the API server's pod proxy wants
// the object.
type Target struct {
	SandboxID string
	Namespace string
	PodName   string
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
	t := Target{
		SandboxID: id,
		Namespace: s.Namespace,
		PodName:   s.Status.PodName,
		PodIP:     s.Status.PodIP,
		Phase:     string(s.Status.Phase),
	}
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
	// Router decides how a request names its sandbox. When nil, requests are
	// routed by hostname over Suffix.
	Router Router
	// Suffix is shorthand for a HostRouter, kept because host routing was the
	// only scheme for most of this package's life.
	Suffix string
	// Transport is how a sandbox Pod is reached. When nil, its IP is dialled.
	Transport Transport
	Log       *slog.Logger

	proxy *httputil.ReverseProxy
	once  sync.Once
}

func (s *Server) transport() Transport {
	if s.Transport != nil {
		return s.Transport
	}
	return DirectTransport{}
}

func (s *Server) router() Router {
	if s.Router != nil {
		return s.Router
	}
	return HostRouter{Suffix: s.Suffix, Resolver: s.Resolver}
}

// proxyRoute is what the Rewrite and ModifyResponse hooks need, carried on the
// request context because httputil gives them nothing else to read.
type proxyRoute struct {
	target Target
	route  Route
	host   string // the Host the client asked for
}

type routeKey struct{}

func routeOf(ctx context.Context) *proxyRoute {
	pr, _ := ctx.Value(routeKey{}).(*proxyRoute)
	return pr
}

func (s *Server) init() {
	s.proxy = &httputil.ReverseProxy{
		Transport: s.transport().RoundTripper(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pt := routeOf(pr.In.Context())
			// The transport owns the URL, the Host and the token, because those
			// three answers change together. It also strips the routing prefix:
			// under path routing the front of the path is ours and means
			// nothing to the sandbox. Both the decoded and raw forms are set,
			// since EscapedPath prefers RawPath and rewriting only the decoded
			// one would turn an escaped separator into a real one on the way in.
			s.transport().Rewrite(pr, pt.target, pt.route)

			// The gateway is the last hop that knows the real client, and the
			// sandbox is untrusted, so forwarding headers are set rather than
			// appended: a value the client supplied must not survive.
			if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set("X-Forwarded-For", ip)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			if pt.route.Prefix != "" {
				// The one thing a sub-path-aware application needs in order to
				// generate its own URLs correctly. Everything else this proxy
				// does for path routing is repair work after the fact.
				pr.Out.Header.Set("X-Forwarded-Prefix", pt.route.Prefix)
			} else {
				pr.Out.Header.Del("X-Forwarded-Prefix")
			}
		},
		// Stream rather than buffer: exec output and PTY sessions are useless
		// if they arrive in blocks.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			pt := routeOf(resp.Request.Context())
			if pt == nil || pt.route.Prefix == "" {
				return nil
			}
			rewriteLocation(resp.Header, pt.route.Prefix, pt.host)
			rewriteCookiePaths(resp.Header, pt.route.Prefix)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.Log.Warn("proxy error", "host", r.Host, "path", r.URL.Path, "err", err)
			writeErr(w, http.StatusBadGateway, "sandbox_unreachable", "the sandbox is not responding")
		},
	}
}

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
	notFound := func() {
		writeErr(w, http.StatusNotFound, "unknown_sandbox",
			"this URL does not correspond to a running sandbox")
	}

	rt, err := s.router().Route(r)
	if err != nil {
		notFound()
		return
	}

	target, ok := s.Resolver.Resolve(r.Context(), rt.SandboxID)
	if !ok {
		notFound()
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

	// A URL that named the sandbox with no trailing slash is redirected rather
	// than forwarded. Serving the sandbox's index page here would leave the
	// browser resolving every relative URL in it one segment too high, and the
	// page would come back a broken skeleton with no clue why.
	if rt.Prefix != "" && rt.Rest == "" {
		to := rt.Prefix + "/"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusMovedPermanently)
		return
	}

	if !rt.UserPort && r.Method == http.MethodPost && rt.Rest == portsPath {
		s.exposePort(w, r, rt)
		return
	}

	if err := s.transport().Reachable(target); err != nil {
		s.Log.Warn("sandbox not reachable", "sandbox", rt.SandboxID, "err", err)
		writeErr(w, http.StatusBadGateway, "sandbox_unreachable", err.Error())
		return
	}
	ctx := context.WithValue(r.Context(), routeKey{}, &proxyRoute{target: target, route: rt, host: r.Host})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// portsPath is the one data-plane route the gateway answers itself.
const portsPath = "/v1/ports"

type exposeRequest struct {
	Port   int  `json:"port"`
	Public bool `json:"public"`
}

type exposeResponse struct {
	Port int    `json:"port"`
	URL  string `json:"url"`
}

// exposePort answers the SDKs' Expose call.
//
// It is handled here rather than forwarded to execd because the answer is a
// URL, and a URL's shape is precisely what execd does not know: nothing ever
// tells it the hostname or the routing scheme it is published under. Both SDKs
// used to derive the URL locally from the endpoint they were given, which was
// wrong the moment a second scheme existed.
//
// There is no allowlist behind this call. Every port on the Pod is already
// reachable to a caller holding the sandbox's token, and an allowlist would
// need durable state that neither the gateway nor execd keeps. What this does
// is tell the caller where to look — no more, and the SDK docs say so.
func (s *Server) exposePort(w http.ResponseWriter, r *http.Request, rt Route) {
	var req exposeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535")
		return
	}
	if req.Port == DataPlanePort {
		writeErr(w, http.StatusBadRequest, "reserved_port",
			"port 8080 is the sandbox's own data plane and is reached at the endpoint itself")
		return
	}
	writeJSON(w, http.StatusOK, exposeResponse{
		Port: req.Port,
		URL:  s.absolute(r, s.router().PortURL(rt.SandboxID, req.Port)),
	})
}

// absolute resolves a reference a Router rendered without knowing the origin.
func (s *Server) absolute(r *http.Request, ref string) string {
	switch {
	case strings.Contains(ref, "://"):
		return ref
	case strings.HasPrefix(ref, "//"):
		return scheme(r) + ":" + ref
	default:
		return scheme(r) + "://" + r.Host + ref
	}
}

// scheme reports what the client actually used. Behind an Ingress the header is
// always set; without one, reporting https for a plaintext local listener would
// hand out URLs that do not work.
func scheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// rewriteLocation puts the routing prefix back on a redirect the sandbox issued.
//
// An application behind path routing has no idea it is behind anything, so it
// answers "Location: /login" and the browser leaves the sandbox altogether.
// This covers redirects only; see the package comment for why response bodies
// are left alone.
func rewriteLocation(h http.Header, prefix, host string) {
	loc := h.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		return
	}
	if u.Host != "" && !strings.EqualFold(u.Host, host) {
		// It points somewhere else entirely, which is the application's
		// business. Rewriting would break more than it fixed.
		return
	}
	if !strings.HasPrefix(u.Path, "/") {
		// Relative to the current directory, so it already resolves against a
		// URL that carries the prefix.
		return
	}
	u.Path = prefix + u.Path
	u.RawPath = "" // let String re-encode from Path
	h.Set("Location", u.String())
}

// rewriteCookiePaths scopes a sandbox's cookies to its own prefix.
//
// Under path routing every sandbox shares one origin, so a cookie set with
// Path=/ is sent to every other sandbox as well and they overwrite each other's
// sessions. This stops that. It is explicitly not an isolation boundary —
// script in one sandbox can still read another's cookies, because it is the
// same origin and the same jar — and nothing here should be mistaken for one.
func rewriteCookiePaths(h http.Header, prefix string) {
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	scoped := make([]string, 0, len(cookies))
	for _, c := range cookies {
		scoped = append(scoped, scopeCookie(c, prefix))
	}
	h.Del("Set-Cookie")
	for _, c := range scoped {
		h.Add("Set-Cookie", c)
	}
}

// scopeCookie edits the Path attribute and leaves every other byte alone.
// Cookies carry attributes this code has never heard of — Partitioned, Priority
// — and re-serialising a parsed cookie silently drops them.
func scopeCookie(cookie, prefix string) string {
	parts := strings.Split(cookie, ";")
	for i := 1; i < len(parts); i++ { // parts[0] is name=value
		name, value, _ := strings.Cut(parts[i], "=")
		if !strings.EqualFold(strings.TrimSpace(name), "path") {
			continue
		}
		p := strings.TrimSpace(value)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		parts[i] = " Path=" + prefix + p
		return strings.Join(parts, ";")
	}
	// No Path attribute means the browser derives one from the request path,
	// which under path routing is the sandbox's prefix plus whatever directory
	// it was serving. Pinning it is both narrower and predictable.
	return cookie + "; Path=" + prefix + "/"
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
