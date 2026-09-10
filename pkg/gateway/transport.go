package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"jdix.io/sandbox/pkg/api"
)

// Transport is how the gateway reaches a sandbox Pod.
//
// It owns the outbound URL, the Host header and whatever the sandbox's token
// has to look like on the wire, because those three answers change together.
// Everything else about the rewrite — forwarding headers, the routing prefix —
// is the same either way and stays in the Server.
type Transport interface {
	// Rewrite points the outbound request at the sandbox.
	Rewrite(pr *httputil.ProxyRequest, target Target, rt Route)
	// RoundTripper carries the request. nil means http.DefaultTransport.
	RoundTripper() http.RoundTripper
	// Reachable reports why a resolved sandbox cannot be reached by this
	// transport, so the failure is a clear 502 rather than a puzzling one from
	// somewhere further along.
	Reachable(target Target) error
}

// DirectTransport dials the Pod IP. This is what production uses.
type DirectTransport struct{}

func (DirectTransport) RoundTripper() http.RoundTripper { return nil }

func (DirectTransport) Reachable(t Target) error {
	if t.PodIP == "" {
		return fmt.Errorf("sandbox %s has no Pod IP yet", t.SandboxID)
	}
	return nil
}

func (DirectTransport) Rewrite(pr *httputil.ProxyRequest, t Target, rt Route) {
	pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(t.PodIP, strconv.Itoa(rt.Port))})
	// The sandbox is told the hostname the client actually asked for, so an
	// application inside it can generate URLs that work.
	pr.Out.Host = pr.In.Host
	pr.Out.URL.Path, pr.Out.URL.RawPath = rt.Rest, rt.RawRest
	presentToken(pr, t, rt, func(token string) {
		pr.Out.Header.Set("Authorization", "Bearer "+token)
	})
}

// APIProxyTransport reaches a sandbox through the Kubernetes API server's pod
// proxy subresource instead of dialling the Pod.
//
// Sandbox Pods have no Service, so the direct transport needs a route to the
// Pod network. A gateway running on a laptop has no such route, and
// `kubectl port-forward` cannot substitute: the gateway proxies to whichever
// Pod a sandbox was bound to, which is not known before the request arrives.
// The API server can already reach every Pod, and a kubeconfig is all this
// needs — which is what makes the data plane debuggable from outside the
// cluster at all.
//
// It is emphatically not the production transport. jdix-controller's equivalent
// turns each bind into one proxied request; this turns every byte of every exec,
// PTY session and file transfer into API server traffic, on the one component
// that must never become the bottleneck.
//
// WebSocket survives the hop, which is not obvious and was measured rather than
// assumed. client-go's transport negotiates HTTP/2 with the API server, and
// HTTP/2 forbids the Connection and Upgrade headers outright — but net/http
// falls back to HTTP/1.1 for a request carrying Upgrade, so the same transport
// answers a plain request over h2 and an upgrade over HTTP/1.1 with a real 101.
// Nothing here has to pin the protocol; something that broke that fallback
// would break every PTY session through this transport.
type APIProxyTransport struct {
	base *url.URL
	rt   http.RoundTripper
}

// NewAPIProxyTransport builds the proxy transport from a REST config.
func NewAPIProxyTransport(cfg *rest.Config) (*APIProxyTransport, error) {
	// The same defaulting client-go applies internally: a kubeconfig may give
	// the host without a scheme, and whether that means https depends on
	// whether any TLS material was configured.
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	defaultTLS := len(cfg.CAFile) != 0 || len(cfg.CAData) != 0 ||
		len(cfg.CertFile) != 0 || len(cfg.CertData) != 0 || cfg.Insecure
	base, _, err := rest.DefaultServerURL(host, "", schema.GroupVersion{}, defaultTLS)
	if err != nil {
		return nil, fmt.Errorf("reading the API server address: %w", err)
	}
	tr, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the API server transport: %w", err)
	}
	return &APIProxyTransport{base: base, rt: tr}, nil
}

func (a *APIProxyTransport) RoundTripper() http.RoundTripper { return a.rt }

func (a *APIProxyTransport) Reachable(t Target) error {
	if t.Namespace == "" || t.PodName == "" {
		return fmt.Errorf("sandbox %s is not bound to a named Pod yet", t.SandboxID)
	}
	return nil
}

func (a *APIProxyTransport) Rewrite(pr *httputil.ProxyRequest, t Target, rt Route) {
	pr.SetURL(a.base)
	// The API server parses "name:port" and assumes https without a scheme
	// prefix, which would fail against the sandbox's plain HTTP listeners.
	prefix := fmt.Sprintf("/api/v1/namespaces/%s/pods/http:%s:%d/proxy",
		t.Namespace, t.PodName, rt.Port)
	pr.Out.URL.Path = prefix + rt.Rest
	pr.Out.URL.RawPath = prefix + rt.RawRest
	// Host stays the API server's: SetURL cleared it, and overriding it with
	// the client's would send the API server a name it does not answer to.

	// Authorization must go regardless. The API server strips it before
	// forwarding, so nothing put there would arrive; and client-go's transport
	// declines to overwrite an Authorization header that is already set, so
	// leaving one would present it to the API server as this gateway's own
	// credential. Either way, a 401 nowhere near its cause.
	presentToken(pr, t, rt, func(token string) {
		// execd accepts the token here precisely because Authorization cannot
		// survive the hop.
		pr.Out.Header.Set(api.ControlTokenHeader, token)
	})
}

// presentToken replaces the caller's credential with the sandbox's own.
//
// The caller authenticated to the gateway with the tenant's API key, which is a
// credential for the whole account: it can create and destroy every sandbox the
// tenant has. It must not travel any further than the gateway. What execd is
// given instead is the per-sandbox token it was told to honour at bind — worth
// one sandbox until its TTL runs out, and useless anywhere else.
//
// This matters most on a user port, where the far end is the tenant's own web
// application, which is to say the untrusted code the sandbox exists to run.
// That code is handed nothing.
func presentToken(pr *httputil.ProxyRequest, t Target, rt Route, set func(string)) {
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del(api.ControlTokenHeader)
	// The credential may also have arrived in the query string, which browsers
	// need for WebSockets. Strip it there too, or it rides into the sandbox in
	// the URL and lands in whatever the application logs.
	if q := pr.Out.URL.Query(); q.Has("token") {
		q.Del("token")
		pr.Out.URL.RawQuery = q.Encode()
	}
	if rt.UserPort || t.Token == "" {
		return
	}
	set(t.Token)
}

var (
	_ Transport = DirectTransport{}
	_ Transport = (*APIProxyTransport)(nil)
)
