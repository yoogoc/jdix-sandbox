package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"jdix.io/sandbox/pkg/api"
	"jdix.io/sandbox/pkg/bwrap"
)

// PodRef says how to reach one sandbox Pod, and how to prove we may.
//
// Both the address and the object identity are carried, because there is more
// than one way to get there: the direct transport dials the IP, while the
// API-proxy transport asks the Kubernetes API server to forward by name. A
// transport that only knew the IP could not offer the second.
//
// Token is that Pod's own control-plane credential, not a platform-wide one.
// It is generated when the Pod is created and read back from the Pod's spec,
// so nothing has to store it and there is no shared secret to keep in sync.
type PodRef struct {
	Namespace string
	Name      string
	IP        string
	Token     string
}

func (p PodRef) String() string {
	if p.Namespace != "" && p.Name != "" {
		return p.Namespace + "/" + p.Name + " (" + p.IP + ")"
	}
	return p.IP
}

// Binder talks to execd's control plane. It is an interface so the reconciler
// can be tested without a running Pod, and so the transport can be swapped —
// see APIProxyExecdClient.
type Binder interface {
	Bind(ctx context.Context, pod PodRef, req api.BindRequest) (api.BindResponse, error)
	Unbind(ctx context.Context, pod PodRef) error
}

// Prober measures a warm Pod before it is allowed to serve anyone.
type Prober interface {
	Probe(ctx context.Context, pod PodRef) (tier bwrap.Tier, reason string, err error)
}

// HTTPExecdClient implements both over the cluster-internal control port.
type HTTPExecdClient struct {
	Port    int32
	Timeout time.Duration
	client  *http.Client
}

func NewHTTPExecdClient() *HTTPExecdClient {
	return &HTTPExecdClient{
		Port:    PortControl,
		Timeout: 20 * time.Second,
		client: &http.Client{
			Transport: &http.Transport{
				// Pods are short-lived and each IP is used briefly, so a large
				// idle pool would just hold connections to Pods that are gone.
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     20 * time.Second,
			},
		},
	}
}

func (c *HTTPExecdClient) url(podIP, path string) string {
	return "http://" + net.JoinHostPort(podIP, fmt.Sprint(c.Port)) + path
}

func (c *HTTPExecdClient) do(ctx context.Context, method, url, token string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr api.Error
		if json.Unmarshal(b, &apiErr) == nil && apiErr.Message != "" {
			// Pass execd's own words through: a rejected filesystem spec should
			// tell the tenant which field was wrong, not just "bind failed".
			return fmt.Errorf("execd: %s: %s", apiErr.Code, apiErr.Message)
		}
		return fmt.Errorf("execd returned %d: %s", resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *HTTPExecdClient) Bind(ctx context.Context, pod PodRef, req api.BindRequest) (api.BindResponse, error) {
	var out api.BindResponse
	err := c.do(ctx, http.MethodPost, c.url(pod.IP, "/internal/v1/bind"), pod.Token, req, &out)
	return out, err
}

func (c *HTTPExecdClient) Unbind(ctx context.Context, pod PodRef) error {
	return c.do(ctx, http.MethodPost, c.url(pod.IP, "/internal/v1/unbind"), pod.Token, nil, nil)
}

func (c *HTTPExecdClient) Probe(ctx context.Context, pod PodRef) (bwrap.Tier, string, error) {
	var out probeResponse
	if err := c.do(ctx, http.MethodGet, c.url(pod.IP, "/internal/v1/probe"), pod.Token, nil, &out); err != nil {
		return "", "", err
	}
	return out.IsolationTier, out.Reason, nil
}

// probeResponse is execd's readiness answer, shared by both transports.
type probeResponse struct {
	IsolationTier bwrap.Tier `json:"isolationTier"`
	Reason        string     `json:"reason"`
}
