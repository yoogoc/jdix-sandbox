package execd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
)

// initTransport dials jdix-init's unix socket. The URL host is a placeholder;
// only the path is used.
func (s *Server) initTransport() *http.Transport {
	sock := path.Join(s.cfg.Layout.IPCDir, bwrap.InitSocketName)
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		// The sandbox is one process on the other end of one socket; pooling
		// beyond a handful of connections buys nothing.
		MaxIdleConns:    8,
		IdleConnTimeout: 30 * time.Second,
	}
}

func (s *Server) initClient() *http.Client {
	return &http.Client{Transport: s.initTransport()}
}

// configureInit hands the tenant's environment and secrets to jdix-init over
// the unix socket. They travel here rather than in the bwrap command line or
// the Pod spec, so they never appear in argv or in `kubectl get pod -o yaml`.
func (s *Server) configureInit(ctx context.Context, req api.BindRequest) error {
	roots := []api.Root{{Path: s.cfg.Layout.WorkspaceDir}}
	// The file API's view must match what bwrap actually mounted, or a tenant
	// could read a path the namespace does not expose.
	for _, m := range req.Filesystem.Mounts {
		roots = append(roots, api.Root{Path: m.Path, ReadOnly: m.ReadOnly})
	}
	ws := req.Filesystem.Workspace.Path
	if ws == "" {
		ws = api.DefaultWorkspacePath
	}
	roots[0].Path = ws

	body, err := json.Marshal(api.ConfigureRequest{
		SandboxID: req.SandboxID,
		Env:       req.Env,
		Secrets:   req.Secrets,
		Roots:     roots,
		Workspace: ws,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://init/configure", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := s.initClient().Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("jdix-init rejected the configuration: %d %s", resp.StatusCode, b)
	}
	return nil
}

// DataHandler is the authenticated surface on :8080, reverse proxied into the
// namespace. Everything under /v1 is jdix-init's; execd only decides whether
// the caller may reach it.
func (s *Server) DataHandler() http.Handler {
	target, _ := url.Parse("http://init")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The token is execd's business; the namespace never sees it.
			pr.Out.Header.Del("Authorization")
			pr.Out.Host = "init"
		},
		Transport: s.initTransport(),
		// FlushInterval -1 streams immediately, which is what exec output and
		// PTY sessions need; buffering would make an interactive shell unusable.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("data plane proxy error", "path", r.URL.Path, "err", err)
			writeErr(w, http.StatusBadGateway, "sandbox_unreachable", "the sandbox is not responding")
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", s.requireSandbox(proxy))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

// requireSandbox gates the data plane. It is the second of the two checks the
// design calls for: the gateway validates the token as well, and execd repeats
// the check so a bypassed gateway does not leave the Pod open.
func (s *Server) requireSandbox(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sb, ok := s.authorised(r)
		if !ok {
			// One response for "not bound", "wrong token" and "expired": a
			// caller without a valid token learns nothing about this Pod.
			writeJSON(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		if !sb.expiresAt.IsZero() && time.Now().After(sb.expiresAt) {
			writeErr(w, http.StatusGone, "expired", "this sandbox has expired")
			return
		}
		if r.URL.Path == "/v1/keepalive" && r.Method == http.MethodPost {
			s.keepalive(w, r, sb)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// keepalive extends the TTL. It is answered by execd rather than proxied,
// because the deadline lives out here with the process that enforces it.
func (s *Server) keepalive(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	extend := 0
	if v := r.URL.Query().Get("ttlSeconds"); v != "" {
		fmt.Sscanf(v, "%d", &extend)
	}
	if extend <= 0 {
		extend = 300
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sb == nil || s.sb.id != sb.id {
		writeErr(w, http.StatusGone, "expired", "this sandbox has expired")
		return
	}
	s.sb.expiresAt = time.Now().Add(time.Duration(extend) * time.Second)
	if s.sb.ttlTimer != nil {
		s.sb.ttlTimer.Reset(time.Until(s.sb.expiresAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expiresAt": s.sb.expiresAt})
}
