package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
)

// APIProxyExecdClient reaches execd through the Kubernetes API server's pod
// proxy subresource instead of dialling the Pod directly.
//
// Sandbox Pods have no Service, so the direct transport needs a route to the
// Pod network. A controller running on a laptop usually has no such route, and
// `kubectl port-forward` cannot help: binding targets whichever Pod the pool
// hands over, and that Pod is not known in advance.
//
// The API server can already reach every Pod, and a kubeconfig is all this
// needs. That makes the whole controller debuggable from outside the cluster.
//
// It is not the production transport. Every bind, unbind and probe becomes a
// request the API server has to proxy, which turns the busiest path in the
// system into load on the one component that must never be the bottleneck.
// Use it for development, and for the occasional cluster whose control plane
// genuinely cannot route to Pods.
type APIProxyExecdClient struct {
	rest    rest.Interface
	Port    int32
	Timeout time.Duration
}

// NewAPIProxyExecdClient builds the proxy transport from a REST config.
func NewAPIProxyExecdClient(cfg *rest.Config) (*APIProxyExecdClient, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &APIProxyExecdClient{
		rest:    cs.CoreV1().RESTClient(),
		Port:    PortControl,
		Timeout: 30 * time.Second,
	}, nil
}

// podName renders the proxy target. The API server parses "name:port", and
// defaults to https without a scheme prefix — which would fail against execd's
// plain HTTP listener, so the scheme is stated.
func (c *APIProxyExecdClient) podName(pod PodRef) string {
	return fmt.Sprintf("http:%s:%d", pod.Name, c.Port)
}

func (c *APIProxyExecdClient) do(ctx context.Context, verb string, pod PodRef, path string, body, out any) error {
	if pod.Namespace == "" || pod.Name == "" {
		return fmt.Errorf("the API-proxy transport needs a Pod name and namespace, got %q", pod)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	var req *rest.Request
	switch verb {
	case http.MethodGet:
		req = c.rest.Get()
	case http.MethodPost:
		req = c.rest.Post()
	default:
		return fmt.Errorf("unsupported verb %q", verb)
	}
	req = req.Namespace(pod.Namespace).
		Resource("pods").
		Name(c.podName(pod)).
		SubResource("proxy").
		Suffix(path)

	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req = req.Body(payload).SetHeader("Content-Type", "application/json")
	}
	// Not Authorization: the API server strips that header from proxied
	// requests, so it never reaches execd and every bind comes back 401. execd
	// accepts this one for exactly that reason.
	//
	// execd authenticates the control plane itself. The API server's own
	// authorisation is separate and does not substitute for it.
	if pod.Token != "" {
		req = req.SetHeader(api.ControlTokenHeader, pod.Token)
	}

	raw, err := req.DoRaw(ctx)
	if err != nil {
		// The proxy hands back execd's body verbatim, so its explanation — which
		// names the offending spec field — survives instead of being flattened
		// into a generic proxy error.
		var apiErr api.Error
		if len(raw) > 0 && json.Unmarshal(raw, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("execd: %s: %s", apiErr.Code, apiErr.Message)
		}
		return fmt.Errorf("pod proxy to %s: %w", pod, err)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *APIProxyExecdClient) Bind(ctx context.Context, pod PodRef, req api.BindRequest) (api.BindResponse, error) {
	var out api.BindResponse
	err := c.do(ctx, http.MethodPost, pod, "internal/v1/bind", req, &out)
	return out, err
}

func (c *APIProxyExecdClient) Unbind(ctx context.Context, pod PodRef) error {
	return c.do(ctx, http.MethodPost, pod, "internal/v1/unbind", nil, nil)
}

func (c *APIProxyExecdClient) Probe(ctx context.Context, pod PodRef) (bwrap.Tier, string, error) {
	var out probeResponse
	if err := c.do(ctx, http.MethodGet, pod, "internal/v1/probe", nil, &out); err != nil {
		return "", "", err
	}
	return out.IsolationTier, out.Reason, nil
}

// Both transports satisfy both interfaces; the compiler says so rather than a
// comment.
var (
	_ Binder = (*APIProxyExecdClient)(nil)
	_ Prober = (*APIProxyExecdClient)(nil)
	_ Binder = (*HTTPExecdClient)(nil)
	_ Prober = (*HTTPExecdClient)(nil)
)
