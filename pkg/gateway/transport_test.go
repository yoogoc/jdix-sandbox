package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/coder/websocket"
	"jdix.io/sandbox/pkg/api"
)

// fakeAPIServer stands in for kube-apiserver's pod proxy subresource.
//
// The one behaviour that matters here is that it strips Authorization before
// forwarding, exactly as the real one does so that a caller's cluster
// credentials never reach a workload. Getting that wrong in a fake is how the
// controller's own proxy transport shipped a 401 that no test caught.
type fakeAPIServer struct {
	*httptest.Server
	lastTarget string // the "http:name:port" segment
	lastAuth   string // what the API server itself was given
	lastNS     string
}

func newFakeAPIServer(t *testing.T, backend *httptest.Server) *fakeAPIServer {
	t.Helper()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPIServer{}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := pr.In.Context().Value(restKey{}).(string)
			pr.SetURL(backendURL)
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, rest
			pr.Out.Header.Del("Authorization") // the whole point of this fake
		},
		FlushInterval: -1,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		esc := r.URL.EscapedPath()
		const lead = "/api/v1/namespaces/"
		if !strings.HasPrefix(esc, lead) {
			http.Error(w, "not a pod proxy path: "+esc, http.StatusNotFound)
			return
		}
		ns, after, ok := strings.Cut(strings.TrimPrefix(esc, lead), "/pods/")
		if !ok {
			http.Error(w, "not a pod proxy path: "+esc, http.StatusNotFound)
			return
		}
		target, rest, ok := strings.Cut(after, "/proxy")
		if !ok {
			http.Error(w, "not a pod proxy path: "+esc, http.StatusNotFound)
			return
		}
		f.lastNS, f.lastTarget = ns, target
		if rest == "" {
			rest = "/"
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), restKey{}, rest)))
	}))
	t.Cleanup(f.Close)
	return f
}

type restKey struct{}

// newProxyGateway wires a gateway that reaches the sandbox through the fake API
// server rather than by dialling it.
func newProxyGateway(t *testing.T, api *fakeAPIServer, target Target) *httptest.Server {
	t.Helper()
	transport, err := NewAPIProxyTransport(&rest.Config{
		Host: api.URL,
		// The gateway's own credential for the API server. It must survive the
		// hop; a sandbox token in Authorization would displace it.
		BearerToken: "kube-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Resolver:  stubResolver{targets: map[string]Target{target.SandboxID: target}},
		Router:    PathRouter{},
		Transport: transport,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func boundTarget(id string) Target {
	return Target{
		SandboxID: id,
		Namespace: "tenant-abc",
		PodName:   "warm-1",
		PodIP:     "10.0.0.9", // present, and deliberately unroutable from here
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestAPIProxyTransportReachesTheSandboxThroughTheAPIServer(t *testing.T) {
	be := newPathBackend(t)
	api := newFakeAPIServer(t, be.Server)
	gw := newProxyGateway(t, api, boundTarget("sbx-abc"))

	resp := get(t, gw, "/s/sbx-abc/v1/exec")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	if api.lastNS != "tenant-abc" {
		t.Errorf("proxied through namespace %q, want tenant-abc", api.lastNS)
	}
	// Without the scheme prefix the API server assumes https and the plaintext
	// data plane refuses the connection.
	if api.lastTarget != "http:warm-1:8080" {
		t.Errorf("pod target %q, want http:warm-1:8080", api.lastTarget)
	}
	if be.lastRawPath != "/v1/exec" {
		t.Errorf("the sandbox saw %q, want /v1/exec", be.lastRawPath)
	}
}

// The two ways this hop turns into a 401, both covered here: the API server
// strips Authorization, and client-go refuses to overwrite one that is already
// set — so a sandbox token left in place would be offered to the API server as
// the gateway's own credential.
func TestAPIProxyTransportMovesTheTokenOffAuthorization(t *testing.T) {
	be := newPathBackend(t)
	var sawAuth, sawToken string
	be.respond = func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawToken = r.Header.Get(api.ControlTokenHeader)
	}
	fake := newFakeAPIServer(t, be.Server)
	gw := newProxyGateway(t, fake, boundTarget("sbx-abc"))

	resp := get(t, gw, "/s/sbx-abc/v1/exec")
	resp.Body.Close()

	if fake.lastAuth != "Bearer kube-token" {
		t.Errorf("the API server was given %q; the gateway's own credential did not survive", fake.lastAuth)
	}
	if sawToken != "sbt_token" {
		t.Errorf("the sandbox was given %q in %s, want the sandbox token", sawToken, api.ControlTokenHeader)
	}
	if sawAuth != "" {
		t.Errorf("Authorization reached the sandbox as %q; the real API server strips it, so relying on it is a 401 in production only", sawAuth)
	}
}

// A tenant's own application has no arrangement with us about custom headers,
// so it is not handed the sandbox's token.
func TestAPIProxyTransportWithholdsTheTokenFromUserPorts(t *testing.T) {
	be := newPathBackend(t)
	var sawToken string
	be.respond = func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get(api.ControlTokenHeader)
	}
	fake := newFakeAPIServer(t, be.Server)
	gw := newProxyGateway(t, fake, boundTarget("sbx-abc"))

	resp := get(t, gw, "/s/sbx-abc/p/8000/index.html")
	resp.Body.Close()

	if fake.lastTarget != "http:warm-1:8000" {
		t.Errorf("pod target %q, want http:warm-1:8000", fake.lastTarget)
	}
	if be.lastRawPath != "/index.html" {
		t.Errorf("the sandbox saw %q, want /index.html", be.lastRawPath)
	}
	if sawToken != "" {
		t.Errorf("a user port was handed the sandbox token as %q", sawToken)
	}
}

// Streaming has to survive two proxies rather than one. This is the reason the
// transport is a rewrite hook on the existing ReverseProxy rather than a
// request/response client like the controller's.
func TestAPIProxyTransportProxiesWebSockets(t *testing.T) {
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
	fake := newFakeAPIServer(t, be.Server)
	gw := newProxyGateway(t, fake, boundTarget("sbx-abc"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/s/sbx-abc/v1/pty",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer sbt_token"}}})
	if err != nil {
		t.Fatalf("dialling through the pod proxy: %v", err)
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
}

// The pod proxy addresses an object, not an address, so a sandbox whose Pod
// name has not been recorded is unreachable — and says which of the two it is.
func TestAPIProxyTransportRefusesASandboxWithNoPodName(t *testing.T) {
	be := newPathBackend(t)
	fake := newFakeAPIServer(t, be.Server)
	target := boundTarget("sbx-abc")
	target.PodName = ""
	gw := newProxyGateway(t, fake, target)

	resp := get(t, gw, "/s/sbx-abc/v1/exec")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if be.lastRawPath != "" {
		t.Error("the request was forwarded anyway")
	}
}

// The direct transport is unaffected by any of the above: it still forwards
// Authorization, which is what execd reads when it is dialled straight.
func TestDirectTransportStillForwardsAuthorization(t *testing.T) {
	be := newPathBackend(t)
	var sawAuth string
	be.respond = func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
	}
	gw, port := newPathGateway(t, "sbx-abc", be)

	resp := get(t, gw, "/s/sbx-abc/p/"+itoa(port)+"/")
	resp.Body.Close()

	if sawAuth != "Bearer sbt_token" {
		t.Errorf("Authorization arrived as %q; execd reads it when dialled directly", sawAuth)
	}
}
