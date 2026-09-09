package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
		"sbx-abc": {SandboxID: "sbx-abc", PodIP: ip, ExpiresAt: time.Now().Add(time.Hour)},
	})
	// The data-plane port is fixed, so the backend has to stand in for it: the
	// user-port form is what lets the test point at an arbitrary port.
	resp := request(t, gw, "sbx-abc-"+itoa(port)+".sbx.example.com", "/v1/exec", "sbt_token")
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
	if be.lastAuth == "" {
		t.Error("execd checks the token too, so it must be forwarded")
	}
}

func TestGatewayRequiresAToken(t *testing.T) {
	be := newBackend(t)
	ip, port := hostPortOf(t, be.Server)
	gw := newGateway(t, map[string]Target{
		"sbx-abc": {SandboxID: "sbx-abc", PodIP: ip, ExpiresAt: time.Now().Add(time.Hour)},
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
		"sbx-abc": {SandboxID: "sbx-abc", PodIP: ip, ExpiresAt: time.Now().Add(time.Hour)},
	})

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/exec", nil)
	req.Host = "sbx-abc-" + itoa(port) + ".sbx.example.com"
	req.Header.Set("Authorization", "Bearer t")
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
		"sbx-old": {SandboxID: "sbx-old", PodIP: "10.0.0.1", ExpiresAt: time.Now().Add(-time.Minute)},
	})

	resp := request(t, gw, "sbx-missing.sbx.example.com", "/v1/exec", "tok")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown sandbox returned %d, want 404", resp.StatusCode)
	}

	resp = request(t, gw, "sbx-old.sbx.example.com", "/v1/exec", "tok")
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
		resp := request(t, gw, host, "/", "tok")
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
