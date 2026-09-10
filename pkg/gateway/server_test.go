package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testKey    = "jdix_sk_0123456789ab_secret"
	testTenant = "t_abc"
	podToken   = "sbt_the_sandboxes_own"
)

// stubAuth stands in for the control plane's authenticator.
type stubAuth struct{ tenant string }

func (a stubAuth) Verify(_ context.Context, presented, _ string) (Caller, error) {
	if presented != testKey {
		return Caller{}, errStubUnauthenticated
	}
	tenant := a.tenant
	if tenant == "" {
		tenant = testTenant
	}
	return Caller{TenantID: tenant, MayUse: true}, nil
}

func (stubAuth) Deny(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
}

var errStubUnauthenticated = errors.New("invalid or missing API key")

func expired(id string) Target {
	t := bound(id, "10.0.0.1")
	t.ExpiresAt = time.Now().Add(-time.Minute)
	return t
}

// bound builds a target the way the resolver would for a running sandbox.
func bound(id, podIP string) Target {
	return Target{
		SandboxID: id, PodIP: podIP, TenantID: testTenant, Token: podToken,
		Namespace: "tenant-abc", PodName: "warm-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// stubResolver stands in for the informer-backed one.
type stubResolver struct{ targets map[string]Target }

func (s stubResolver) Resolve(_ context.Context, id string) (Target, bool) {
	t, ok := s.targets[id]
	return t, ok && t.PodIP != ""
}

// backend records what the sandbox Pod would have received.
type backend struct {
	*httptest.Server
	lastPath string
	lastHost string
	lastXFF  string
	lastAuth string
}

func newBackend(t *testing.T) *backend {
	b := &backend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.lastPath = r.URL.Path
		b.lastHost = r.Host
		b.lastXFF = r.Header.Get("X-Forwarded-For")
		b.lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "from the sandbox")
	}))
	t.Cleanup(b.Close)
	return b
}

func newGateway(t *testing.T, targets map[string]Target) *httptest.Server {
	t.Helper()
	srv := &Server{
		Resolver: stubResolver{targets: targets},
		Suffix:   "sbx.example.com",
		Auth:     stubAuth{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func request(t *testing.T, gw *httptest.Server, host, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, gw.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func hostPortOf(t *testing.T, s *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmtSscan(u.Port(), &port); err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), port
}

func TestGatewayProxiesToTheSandbox(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)

	gw := newGateway(t, map[string]Target{
		"sbx-abc": bound("sbx-abc", ip),
	})
	// The data-plane port is fixed, so the backend has to stand in for it: the
	// user-port form is what lets the test point at an arbitrary port.
	resp := request(t, gw, "sbx-abc-"+itoa(port)+".sbx.example.com", "/v1/exec", testKey)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if string(body) != "from the sandbox" {
		t.Fatalf("body %q", body)
	}
	if be.lastPath != "/v1/exec" {
		t.Errorf("path arrived as %q", be.lastPath)
	}
	// This is the user-port form, so the far end is the tenant's own web
	// application — the untrusted code the sandbox exists to run. It is given
	// no credential at all, least of all the account-wide one the caller used.
	if be.lastAuth != "" {
		t.Errorf("a user port was handed %q", be.lastAuth)
	}
}

// stubRouter reports a fixed route, so a test can exercise the data plane
// against a backend that is not on port 8080.
type stubRouter struct{ port int }

func (r stubRouter) Route(req *http.Request) (Route, error) {
	return Route{SandboxID: "sbx-abc", Port: r.port, Rest: req.URL.Path, RawRest: req.URL.EscapedPath()}, nil
}
func (stubRouter) EndpointURL(string) string  { return "" }
func (stubRouter) PortURL(string, int) string { return "" }

// The caller's API key stops at the gateway. execd is given the per-sandbox
// token instead: worth one sandbox until its TTL runs out, and useless
// anywhere else, where the key it replaces can create and destroy every
// sandbox the tenant has.
func TestGatewayPresentsTheSandboxsOwnTokenToExecd(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	srv := &Server{
		Resolver: stubResolver{targets: map[string]Target{"sbx-abc": bound("sbx-abc", ip)}},
		Router:   stubRouter{port: port},
		Auth:     stubAuth{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp := request(t, gw, "anything", "/v1/exec", testKey)
	resp.Body.Close()

	if be.lastAuth != "Bearer "+podToken {
		t.Errorf("the sandbox was given %q, want the per-sandbox token", be.lastAuth)
	}
	if strings.Contains(be.lastAuth, testKey) {
		t.Fatal("the tenant's API key reached the sandbox")
	}
}

// A credential in the query string is what browsers must use for WebSockets,
// and it would otherwise ride into the sandbox in the URL and land in whatever
// the application logs.
func TestGatewayStripsTheCallersTokenFromTheQuery(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	var sawQuery string
	be.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		be.lastAuth = r.Header.Get("Authorization")
	})
	srv := &Server{
		Resolver: stubResolver{targets: map[string]Target{"sbx-abc": bound("sbx-abc", ip)}},
		Router:   stubRouter{port: port},
		Auth:     stubAuth{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp := request(t, gw, "anything", "/v1/pty?token="+testKey+"&rows=40", "")
	resp.Body.Close()

	if strings.Contains(sawQuery, testKey) {
		t.Errorf("the API key rode into the sandbox in the query string: %q", sawQuery)
	}
	if !strings.Contains(sawQuery, "rows=40") {
		t.Errorf("the rest of the query was lost: %q", sawQuery)
	}
	if be.lastAuth != "Bearer "+podToken {
		t.Errorf("the sandbox was given %q", be.lastAuth)
	}
}

// An API key is tenant-wide. Without an ownership check it would open any
// sandbox in the cluster, which is the one thing this change must not cost.
func TestGatewayRefusesASandboxBelongingToAnotherTenant(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	srv := &Server{
		Resolver: stubResolver{targets: map[string]Target{"sbx-abc": bound("sbx-abc", ip)}},
		Router:   stubRouter{port: port},
		Auth:     stubAuth{tenant: "t_someone_else"},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	resp := request(t, gw, "anything", "/v1/exec", testKey)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if be.lastPath != "" {
		t.Error("another tenant's key reached the sandbox")
	}
}

func TestGatewayRequiresAToken(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	gw := newGateway(t, map[string]Target{
		"sbx-abc": bound("sbx-abc", ip),
	})

	resp := request(t, gw, "sbx-abc-"+itoa(port)+".sbx.example.com", "/v1/exec", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if be.lastPath != "" {
		t.Error("an unauthenticated request reached the sandbox")
	}
}

func TestGatewayOverwritesClientSuppliedForwardingHeaders(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	gw := newGateway(t, map[string]Target{
		"sbx-abc": bound("sbx-abc", ip),
	})

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/exec", nil)
	req.Host = "sbx-abc-" + itoa(port) + ".sbx.example.com"
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Forwarded-For", "203.0.113.9") // a lie from the client
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.Contains(be.lastXFF, "203.0.113.9") {
		t.Fatalf("the client's own X-Forwarded-For survived: %q", be.lastXFF)
	}
}

func TestGatewayRejectsUnknownAndExpiredSandboxes(t *testing.T) {
	gw := newGateway(t, map[string]Target{
		"sbx-old": expired("sbx-old"),
	})

	resp := request(t, gw, "sbx-missing.sbx.example.com", "/v1/exec", testKey)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown sandbox returned %d, want 404", resp.StatusCode)
	}

	resp = request(t, gw, "sbx-old.sbx.example.com", "/v1/exec", testKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("expired sandbox returned %d, want 410", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != "expired" {
		t.Errorf("error code %v, want expired", body["code"])
	}
}

func TestGatewayIgnoresForeignHostnames(t *testing.T) {
	gw := newGateway(t, map[string]Target{})
	for _, host := range []string{"evil.com", "sbx-abc.other.example.com", "sbx.example.com"} {
		resp := request(t, gw, host, "/", testKey)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("host %q returned %d, want 404", host, resp.StatusCode)
		}
	}
}

func TestGatewayHealthzNeedsNoToken(t *testing.T) {
	gw := newGateway(t, map[string]Target{})
	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz returned %d", resp.StatusCode)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNoRoute
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}

// Answering 404 for an unknown id and 410 for an expired one, to someone who
// has shown no credential, tells them which sandbox ids exist. Authentication
// comes first, so an anonymous caller learns the same thing either way.
func TestGatewayRevealsNothingBeforeAuthenticating(t *testing.T) {
	gw := newGateway(t, map[string]Target{"sbx-old": expired("sbx-old")})

	for _, host := range []string{"sbx-old.sbx.example.com", "sbx-missing.sbx.example.com"} {
		resp := request(t, gw, host, "/v1/exec", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s returned %d without a credential, want 401", host, resp.StatusCode)
		}
	}
}
