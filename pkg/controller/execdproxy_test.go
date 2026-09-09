package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
)

// fakeAPIServer stands in for the Kubernetes API server, recording the proxy
// path it is asked for. The path format ("http:name:port" plus the proxy
// subresource) is easy to get subtly wrong and impossible to notice until a
// bind fails against a real cluster, so it is asserted directly.
type fakeAPIServer struct {
	*httptest.Server
	lastPath       string
	lastMethod     string
	lastAuth       string
	forwardedToken string
	lastBody       []byte
	status         int
	response       any
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	f := &fakeAPIServer{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath, f.lastMethod = r.URL.Path, r.Method
		// The real API server strips Authorization before forwarding to a Pod,
		// so that a caller's cluster credentials never reach the workload.
		// Echoing it back instead — as this fake once did — hides the fact that
		// nothing authenticated arrives, and the transport looks correct while
		// every bind fails against a real cluster.
		f.lastAuth = r.Header.Get("Authorization")
		f.forwardedToken = r.Header.Get(api.ControlTokenHeader)
		f.lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if f.response != nil {
			_ = json.NewEncoder(w).Encode(f.response)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func newProxyClient(t *testing.T, f *fakeAPIServer) *APIProxyExecdClient {
	t.Helper()
	c, err := NewAPIProxyExecdClient(&rest.Config{Host: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testPod() PodRef {
	return PodRef{Namespace: testNS, Name: "warm-1", IP: "10.0.0.5", Token: "jct_warm-1"}
}

func TestProxyBindUsesThePodProxySubresource(t *testing.T) {
	f := newFakeAPIServer(t)
	f.response = api.BindResponse{SandboxID: "sbx-1", IsolationTier: string(bwrap.TierUserns)}
	c := newProxyClient(t, f)

	resp, err := c.Bind(context.Background(), testPod(), api.BindRequest{SandboxID: "sbx-1", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SandboxID != "sbx-1" {
		t.Fatalf("response: %+v", resp)
	}

	// The scheme prefix matters: without it the API server proxies over https
	// and execd's plain listener rejects the connection.
	want := "/api/v1/namespaces/" + testNS + "/pods/http:warm-1:8081/proxy/internal/v1/bind"
	if f.lastPath != want {
		t.Fatalf("proxy path\n  got  %s\n  want %s", f.lastPath, want)
	}
	if f.lastMethod != http.MethodPost {
		t.Errorf("method %s", f.lastMethod)
	}
	// The credential is this Pod's, not a platform-wide one, and it has to
	// survive the proxy hop or execd rejects the bind.
	if f.forwardedToken != "jct_warm-1" {
		t.Errorf("the Pod's token did not arrive in %s, got %q", api.ControlTokenHeader, f.forwardedToken)
	}
	// It must not be sent as Authorization: the API server would strip that,
	// and the request would arrive with no credential at all.
	if f.lastAuth != "" {
		t.Errorf("the token was sent in Authorization (%q), which the API server strips", f.lastAuth)
	}
	var sent api.BindRequest
	if err := json.Unmarshal(f.lastBody, &sent); err != nil || sent.SandboxID != "sbx-1" {
		t.Errorf("body did not survive: %s", f.lastBody)
	}
}

func TestProxyProbeAndUnbind(t *testing.T) {
	f := newFakeAPIServer(t)
	c := newProxyClient(t, f)

	f.response = probeResponse{IsolationTier: bwrap.TierUserns, Reason: "measured"}
	tier, reason, err := c.Probe(context.Background(), testPod())
	if err != nil {
		t.Fatal(err)
	}
	if tier != bwrap.TierUserns || reason != "measured" {
		t.Fatalf("probe: %q %q", tier, reason)
	}
	if !strings.HasSuffix(f.lastPath, "/proxy/internal/v1/probe") || f.lastMethod != http.MethodGet {
		t.Fatalf("probe request: %s %s", f.lastMethod, f.lastPath)
	}

	f.response = map[string]any{"ok": true}
	if err := c.Unbind(context.Background(), testPod()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.lastPath, "/proxy/internal/v1/unbind") {
		t.Fatalf("unbind path: %s", f.lastPath)
	}
}

// A rejected filesystem spec has to reach the tenant intact. If the proxy
// flattened it into "the request failed", every bad mount would look the same.
func TestProxyPreservesExecdsExplanation(t *testing.T) {
	f := newFakeAPIServer(t)
	f.status = http.StatusBadRequest
	f.response = api.Error{
		Code:    "bind_failed",
		Message: `filesystem.mounts[0].path "/opt/jdix": overlaps platform path /opt/jdix`,
	}
	c := newProxyClient(t, f)

	_, err := c.Bind(context.Background(), testPod(), api.BindRequest{SandboxID: "x", Token: "t"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "filesystem.mounts[0].path") {
		t.Fatalf("execd's explanation was lost: %v", err)
	}
}

// The direct transport only needs an IP; this one needs the object's identity,
// and failing clearly beats issuing a request to "/pods/:8081/proxy".
func TestProxyRequiresPodIdentity(t *testing.T) {
	f := newFakeAPIServer(t)
	c := newProxyClient(t, f)

	_, err := c.Bind(context.Background(), PodRef{IP: "10.0.0.5"}, api.BindRequest{SandboxID: "x", Token: "t"})
	if err == nil || !strings.Contains(err.Error(), "name and namespace") {
		t.Fatalf("expected a clear error about the missing identity, got %v", err)
	}
	if f.lastPath != "" {
		t.Error("no request should have been sent")
	}
}

func TestPodRefString(t *testing.T) {
	if got := (PodRef{Namespace: "ns", Name: "p", IP: "1.2.3.4"}).String(); got != "ns/p (1.2.3.4)" {
		t.Errorf("got %q", got)
	}
	if got := (PodRef{IP: "1.2.3.4"}).String(); got != "1.2.3.4" {
		t.Errorf("got %q", got)
	}
}
