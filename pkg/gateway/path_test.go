package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPathRouterParsesRequests(t *testing.T) {
	cases := []struct {
		path     string
		wantID   string
		wantPort int
		wantUser bool
		wantRest string
		wantErr  bool
	}{
		{"/s/sbx-abc/v1/exec", "sbx-abc", DataPlanePort, false, "/v1/exec", false},
		{"/s/sbx-abc/", "sbx-abc", DataPlanePort, false, "/", false},
		// No trailing slash: routed, but Rest is empty so the server redirects.
		{"/s/sbx-abc", "sbx-abc", DataPlanePort, false, "", false},
		{"/s/sbx-abc/p/8000/static/app.js", "sbx-abc", 8000, true, "/static/app.js", false},
		{"/s/sbx-abc/p/8000/", "sbx-abc", 8000, true, "/", false},
		{"/s/sbx-abc/p/8000", "sbx-abc", 8000, true, "", false},
		// An id that ends in something numeric-looking needs no disambiguation
		// here, which is the whole point of putting the port in its own segment.
		{"/s/sbx-with-8000/v1/exec", "sbx-with-8000", DataPlanePort, false, "/v1/exec", false},
		// "p" only means a port when a valid one follows it.
		{"/s/sbx-abc/p/notaport/x", "", 0, false, "", true},
		{"/s/sbx-abc/p/99999/x", "", 0, false, "", true},
		{"/s/sbx-abc/p/0/x", "", 0, false, "", true},
		// A path segment called "p" that is not a port prefix stays the
		// sandbox's own. The data plane lives under /v1/, so this cannot bite.
		{"/s/sbx-abc/people", "sbx-abc", DataPlanePort, false, "/people", false},
		{"/v1/sandboxes", "", 0, false, "", true}, // the control plane's own surface
		{"/s", "", 0, false, "", true},
		{"/s/", "", 0, false, "", true},
		{"/", "", 0, false, "", true},
	}
	var router PathRouter
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		got, err := router.Route(r)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Route(%q) = %+v, want an error", tc.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Route(%q): %v", tc.path, err)
			continue
		}
		if got.SandboxID != tc.wantID || got.Port != tc.wantPort ||
			got.UserPort != tc.wantUser || got.Rest != tc.wantRest {
			t.Errorf("Route(%q) = id=%s port=%d user=%v rest=%q, want id=%s port=%d user=%v rest=%q",
				tc.path, got.SandboxID, got.Port, got.UserPort, got.Rest,
				tc.wantID, tc.wantPort, tc.wantUser, tc.wantRest)
		}
	}
}

// TestRoutersParseWhatTheyRender is the property that keeps the controller and
// the gateway from drifting: whatever URL is written to status.endpoint has to
// route back to the sandbox it names.
func TestRoutersParseWhatTheyRender(t *testing.T) {
	routers := map[string]Router{
		"path":          PathRouter{Base: "https://api.example.com"},
		"path/prefix":   PathRouter{Prefix: "sandbox", Base: "https://api.example.com"},
		"host":          HostRouter{Suffix: "sbx.example.com", Scheme: "https"},
		"chain":         ChainRouter{PathRouter{Base: "https://api.example.com"}, HostRouter{Suffix: "sbx.example.com"}},
		"path/nobase":   PathRouter{},
		"host/noscheme": HostRouter{Suffix: "sbx.example.com"},
	}
	const id = "sbx-abc123"

	for name, router := range routers {
		// The SDK appends a data-plane path to whatever endpoint it was given.
		endpoint := router.EndpointURL(id) + "/v1/exec"
		assertRoutesTo(t, name+" endpoint", router, endpoint, id, DataPlanePort, false)

		port := router.PortURL(id, 8000)
		assertRoutesTo(t, name+" port", router, port, id, 8000, true)
	}
}

func assertRoutesTo(t *testing.T, what string, router Router, rendered, wantID string, wantPort int, wantUser bool) {
	t.Helper()
	u, err := url.Parse(rendered)
	if err != nil {
		t.Fatalf("%s: %q is not a URL: %v", what, rendered, err)
	}
	r := httptest.NewRequest(http.MethodGet, cmpOr(u.EscapedPath(), "/"), nil)
	if u.Host != "" {
		r.Host = u.Host
	}
	got, err := router.Route(r)
	if err != nil {
		t.Errorf("%s: rendered %q, which its own router refuses: %v", what, rendered, err)
		return
	}
	if got.SandboxID != wantID || got.Port != wantPort || got.UserPort != wantUser {
		t.Errorf("%s: rendered %q, which routes to id=%s port=%d user=%v; want id=%s port=%d user=%v",
			what, rendered, got.SandboxID, got.Port, got.UserPort, wantID, wantPort, wantUser)
	}
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// pathBackend stands in for a sandbox Pod and records what reached it.
type pathBackend struct {
	*httptest.Server
	lastRawPath string
	lastPrefix  string
	// respond, when set, writes the response instead of the default.
	respond func(w http.ResponseWriter, r *http.Request)
}

func newPathBackend(t *testing.T) *pathBackend {
	b := &pathBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.lastRawPath = r.URL.EscapedPath()
		b.lastPrefix = r.Header.Get("X-Forwarded-Prefix")
		if b.respond != nil {
			b.respond(w, r)
			return
		}
		_, _ = io.WriteString(w, "from the sandbox")
	}))
	t.Cleanup(b.Close)
	return b
}

// newPathGateway points the given sandbox id at the backend, on the port the
// backend happens to be listening on, so a user-port route reaches it.
func newPathGateway(t *testing.T, id string, be *pathBackend) (*httptest.Server, int) {
	t.Helper()
	ip, port := hostPortOf(t, be.Server)
	srv := &Server{
		Resolver: stubResolver{targets: map[string]Target{
			id: {SandboxID: id, PodIP: ip, ExpiresAt: time.Now().Add(time.Hour)},
		}},
		Router: PathRouter{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, port
}

// get issues a request that does not follow redirects, so the redirect itself
// can be asserted on.
func get(t *testing.T, gw *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, gw.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sbt_token")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGatewayStripsThePrefixBeforeProxying(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)

	resp := get(t, gw, "/s/sbx-abc/p/"+strconv.Itoa(port)+"/static/app.js")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if be.lastRawPath != "/static/app.js" {
		t.Errorf("the sandbox saw %q; the routing prefix is not its business", be.lastRawPath)
	}
	if be.lastPrefix != "/s/sbx-abc/p/"+strconv.Itoa(port) {
		t.Errorf("X-Forwarded-Prefix = %q, so a sub-path-aware app cannot build its own URLs", be.lastPrefix)
	}
}

// An escaped separator survives the prefix strip. Rewriting only the decoded
// path would hand the sandbox a directory boundary the caller never wrote.
func TestGatewayPreservesEscapedSeparators(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)

	resp := get(t, gw, "/s/sbx-abc/p/"+strconv.Itoa(port)+"/files/a%2Fb")
	resp.Body.Close()

	if be.lastRawPath != "/files/a%2Fb" {
		t.Errorf("the sandbox saw %q, want /files/a%%2Fb", be.lastRawPath)
	}
}

func TestGatewayRedirectsWhenTheTrailingSlashIsMissing(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)
	prefix := "/s/sbx-abc/p/" + strconv.Itoa(port)

	resp := get(t, gw, prefix+"?a=1")
	resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != prefix+"/?a=1" {
		t.Errorf("Location = %q, want %q", got, prefix+"/?a=1")
	}
	if be.lastRawPath != "" {
		t.Error("the request was forwarded; every relative URL in the response would resolve one segment too high")
	}
}

func TestGatewayRewritesRedirectsBackIntoTheSandbox(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)
	prefix := "/s/sbx-abc/p/" + strconv.Itoa(port)

	cases := []struct{ from, want string }{
		{"/login", prefix + "/login"},
		{"/login?next=/x", prefix + "/login?next=/x"},
		{"relative", "relative"},                           // already resolves against the prefixed URL
		{"https://example.com/x", "https://example.com/x"}, // somewhere else entirely
	}
	for _, tc := range cases {
		be.respond = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", tc.from)
			w.WriteHeader(http.StatusFound)
		}
		resp := get(t, gw, prefix+"/")
		resp.Body.Close()
		if got := resp.Header.Get("Location"); got != tc.want {
			t.Errorf("Location %q rewritten to %q, want %q", tc.from, got, tc.want)
		}
	}
}

func TestGatewayScopesCookiesToTheSandbox(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)
	prefix := "/s/sbx-abc/p/" + strconv.Itoa(port)

	be.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sid=1; Path=/; HttpOnly; SameSite=Lax; Partitioned")
		w.Header().Add("Set-Cookie", "pref=dark")
		w.Header().Add("Set-Cookie", "deep=1; path=/admin; Secure")
	}
	resp := get(t, gw, prefix+"/")
	resp.Body.Close()

	got := resp.Header.Values("Set-Cookie")
	want := []string{
		"sid=1; Path=" + prefix + "/; HttpOnly; SameSite=Lax; Partitioned",
		"pref=dark; Path=" + prefix + "/",
		"deep=1; Path=" + prefix + "/admin; Secure",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cookies, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cookie %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Attributes this code has never heard of have to survive verbatim.
	if !strings.Contains(got[0], "Partitioned") {
		t.Error("Partitioned was dropped; re-serialising a parsed cookie loses attributes")
	}
}

func TestExposeAnswersWithARoutableURL(t *testing.T) {
	be := newPathBackend(t)
	gw, _ := newPathGateway(t, "sbx-abc", be)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/s/sbx-abc/v1/ports",
		strings.NewReader(`{"port":8000,"public":true}`))
	req.Header.Set("Authorization", "Bearer sbt_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var out struct {
		Port int    `json:"port"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Port != 8000 {
		t.Errorf("port %d, want 8000", out.Port)
	}
	want := gw.URL + "/s/sbx-abc/p/8000/"
	if out.URL != want {
		t.Errorf("url %q, want %q", out.URL, want)
	}
	if be.lastRawPath != "" {
		t.Error("the call was forwarded to execd, which has no idea what hostname it is published under")
	}
}

func TestExposeRefusesTheDataPlanePort(t *testing.T) {
	be := newPathBackend(t)
	gw, _ := newPathGateway(t, "sbx-abc", be)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/s/sbx-abc/v1/ports",
		strings.NewReader(`{"port":8080}`))
	req.Header.Set("Authorization", "Bearer sbt_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestPathGatewayStillRequiresAToken(t *testing.T) {
	be := newPathBackend(t)
	gw, port := newPathGateway(t, "sbx-abc", be)

	resp, err := http.Get(gw.URL + "/s/sbx-abc/p/" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if be.lastRawPath != "" {
		t.Error("an unauthenticated request reached the sandbox")
	}
}

func TestPathGatewayIgnoresTheControlPlaneSurface(t *testing.T) {
	be := newPathBackend(t)
	gw, _ := newPathGateway(t, "sbx-abc", be)

	for _, path := range []string{"/v1/sandboxes", "/", "/s", "/s/sbx-missing/v1/exec"} {
		resp := get(t, gw, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, resp.StatusCode)
		}
	}
}

// Streaming is what the data plane is for, so the prefix strip has to survive
// an upgrade as well as a plain request. It is also the case that breaks most
// visibly in production and least visibly in a unit test.
func TestGatewayProxiesWebSocketsUnderAPrefix(t *testing.T) {
	be := newPathBackend(t)
	be.respond = func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, append([]byte("echo:"), msg...))
	}
	// The user-port form, so the proxy dials the backend rather than the fixed
	// data-plane port, which on a developer's machine may well be in use.
	gw, port := newPathGateway(t, "sbx-abc", be)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(gw.URL, "http") +
		"/s/sbx-abc/p/" + strconv.Itoa(port) + "/v1/pty"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sbt_token"}},
	})
	if err != nil {
		t.Fatalf("dialing %s: %v", wsURL, err)
	}
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "echo:hello" {
		t.Errorf("read %q, want echo:hello", msg)
	}
	if be.lastRawPath != "/v1/pty" {
		t.Errorf("the sandbox saw %q, want /v1/pty", be.lastRawPath)
	}
}
