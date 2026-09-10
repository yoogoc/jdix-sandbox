package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"jdix.io/sandbox/pkg/api"
	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

// Server is the tenant-facing REST API.
//
// It creates Sandbox objects and waits for the controller to report them ready.
// It never touches a Pod: one component owns binding, and this is not it.
type Server struct {
	Client client.Client
	// APIReader reads straight from the Kubernetes API server, bypassing the
	// informer cache.
	//
	// Client is cache-backed, which is what makes the readiness poll nearly
	// free — but writes go straight to the API server while reads come from a
	// cache that lags behind it by however long a watch event takes to arrive.
	// A create followed immediately by a read therefore sometimes reports the
	// object as missing, which is the one answer that cannot be true: we just
	// made it. This reader is consulted when that happens.
	APIReader client.Reader
	Store     Store
	Auth      *Authenticator
	Log       *slog.Logger

	// ReadyTimeout bounds how long a create call waits before handing back a
	// pending sandbox for the SDK to poll. Holding the connection longer helps
	// nobody: a cold start can take seconds and the client has better things to
	// do than keep a socket open for them.
	ReadyTimeout time.Duration
	// ReadyPoll is the interval between readiness checks. These reads hit the
	// manager's informer cache, not the API server, so they are nearly free.
	ReadyPoll time.Duration

	NewID func() string
	Now   func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return "sbx-" + strings.ToLower(randomSuffix())
}

// Route is one entry of the public API surface.
//
// The routes are data rather than a sequence of mux calls so that the OpenAPI
// document and the server cannot drift apart: a test walks this table and the
// spec and insists they describe the same thing. net/http's ServeMux does not
// expose what was registered, so without a table there is nothing to compare.
type Route struct {
	Method  string
	Path    string
	Scope   Scope
	Handler http.HandlerFunc
}

// Routes is the authenticated API surface. /healthz is deliberately absent: it
// is infrastructure, not part of the contract the SDKs are generated from.
func (s *Server) Routes() []Route {
	return []Route{
		{"POST", "/v1/sandboxes", ScopeSandboxCreate, s.createSandbox},
		{"GET", "/v1/sandboxes", ScopeSandboxRead, s.listSandboxes},
		{"GET", "/v1/sandboxes/{id}", ScopeSandboxRead, s.getSandbox},
		{"DELETE", "/v1/sandboxes/{id}", ScopeSandboxDelete, s.deleteSandbox},

		{"GET", "/v1/templates", ScopeTemplateRead, s.listTemplates},
		{"POST", "/v1/templates/{name}/warmup-request", ScopeTemplateRead, s.requestWarmup},

		{"GET", "/v1/quota", ScopeSandboxRead, s.getQuota},

		// Warm capacity is the platform's money, so allocating it takes the
		// admin scope: a tenant key can ask, never grant.
		{"GET", "/v1/admin/warmup-requests", ScopeAdmin, s.listWarmupRequests},
		{"POST", "/v1/admin/warmup-requests/{id}/review", ScopeAdmin, s.reviewWarmup},
		{"GET", "/v1/admin/budget", ScopeAdmin, s.getBudget},
	}
}

// Handler builds the routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.Routes() {
		mux.Handle(r.Method+" "+r.Path, RequireScope(r.Scope, r.Handler))
	}
	authed := s.Auth.Authenticate(mux)

	root := http.NewServeMux()
	root.Handle("/v1/", authed)
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return root
}

// CreateSandboxRequest is the public create body.
type CreateSandboxRequest struct {
	Template   string              `json:"template"`
	TTLSeconds int                 `json:"ttlSeconds,omitempty"`
	Filesystem *api.FilesystemSpec `json:"filesystem,omitempty"`
	Env        map[string]string   `json:"env,omitempty"`
	Metadata   map[string]string   `json:"metadata,omitempty"`
}

// SandboxResponse is what the SDKs consume.
type SandboxResponse struct {
	ID            string     `json:"id"`
	State         string     `json:"state"`
	Template      string     `json:"template"`
	IsolationTier string     `json:"isolationTier,omitempty"`
	Endpoint      string     `json:"endpoint,omitempty"`
	Token         string     `json:"token,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	ColdStart     bool       `json:"coldStart"`
	Reason        string     `json:"reason,omitempty"`
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())

	var req CreateSandboxRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Template == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "template is required")
		return
	}
	if !p.Key.AllowsTemplate(req.Template) {
		writeErr(w, http.StatusForbidden, "template_not_allowed",
			"this API key is restricted to a different set of templates")
		return
	}

	// Idempotency comes before the quota check: a retry must not be counted
	// twice, and it must not be rejected because the first attempt filled the
	// last slot.
	idemKey := r.Header.Get("Idempotency-Key")
	sum := requestSum(req)
	if idemKey != "" {
		prev, err := s.Store.Idempotency(r.Context(), p.TenantID, idemKey)
		if err == nil {
			if prev.RequestSum != sum {
				writeErr(w, http.StatusConflict, "idempotency_mismatch",
					"this Idempotency-Key was used with a different request body")
				return
			}
			s.respondWithSandbox(w, r, p, prev.SandboxID, "")
			return
		}
		if !errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
	}

	if err := s.checkQuota(r.Context(), p); err != nil {
		if qe, ok := errors.AsType[*quotaError](err); ok {
			w.Header().Set("Retry-After", strconv.Itoa(qe.RetryAfterSeconds))
			writeErr(w, http.StatusTooManyRequests, qe.Code, qe.Message)
			return
		}
		writeErr(w, http.StatusInternalServerError, "quota_check_failed", err.Error())
		return
	}

	id := s.newID()
	sbx := &sbxv1.Sandbox{
		Name:      id,
		Namespace: p.Namespace,
		Labels: map[string]string{
			sbxv1.LabelTenant:   p.TenantID,
			sbxv1.LabelTemplate: req.Template,
		},
		Spec: sbxv1.SandboxSpec{
			TemplateRef: req.Template,
			TTLSeconds:  int32(req.TTLSeconds),
			Metadata:    req.Metadata,
		},
	}
	if req.Filesystem != nil {
		sbx.Spec.Filesystem = toCRDFilesystem(*req.Filesystem)
	}
	for k, v := range req.Env {
		sbx.Spec.Env = append(sbx.Spec.Env, sbxv1.EnvVar{Name: k, Value: v})
	}

	if err := s.Client.Create(r.Context(), sbx); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusBadRequest, "template_not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}

	_ = s.Store.RecordSandbox(r.Context(), &SandboxRecord{
		ID: id, TenantID: p.TenantID, APIKeyID: p.KeyID,
		Template: req.Template, State: string(sbxv1.PhasePending),
		CreatedAt: s.now(), Metadata: req.Metadata,
	})
	_ = s.Store.Audit(r.Context(), AuditEvent{
		TenantID: p.TenantID, Actor: p.KeyID, Action: "sandbox.create", Target: id,
		IP: clientIP(r), UserAgent: r.UserAgent(), At: s.now(),
	})
	if idemKey != "" {
		_ = s.Store.SaveIdempotency(r.Context(), &IdempotencyRecord{
			Key: idemKey, TenantID: p.TenantID, RequestSum: sum, SandboxID: id, CreatedAt: s.now(),
		})
	}

	s.respondWithSandbox(w, r, p, id, "")
}

// respondWithSandbox waits briefly for the controller to finish, then answers
// with whatever state the sandbox has reached.
func (s *Server) respondWithSandbox(w http.ResponseWriter, r *http.Request, p *Principal, id, _ string) {
	sbx, err := s.waitReady(r.Context(), p.Namespace, id)
	if apierrors.IsNotFound(err) {
		// Reached through the idempotency path, for a sandbox that has since
		// been deleted. 404 says that; a 500 would blame the wrong thing.
		writeErr(w, http.StatusNotFound, "not_found", "this sandbox no longer exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	resp := toResponse(sbx)
	resp.Token = sbx.Status.Token
	code := http.StatusCreated
	if sbx.Status.Phase == sbxv1.PhaseFailed {
		// The sandbox exists and its object records why it failed, so this is a
		// 200 describing a failure rather than an error about the request.
		code = http.StatusOK
	}
	writeJSON(w, code, resp)
}

// fetchSandbox reads a Sandbox, falling back to an uncached read when the cache
// says it is missing.
//
// Only the NotFound case falls through: every other error means the read itself
// failed, and asking again a different way would not help. The extra API call
// happens on that one path, not on the hot one.
func (s *Server) fetchSandbox(ctx context.Context, ns, name string, out *sbxv1.Sandbox) error {
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, out)
	if !apierrors.IsNotFound(err) || s.APIReader == nil {
		return err
	}
	return s.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, out)
}

func (s *Server) waitReady(ctx context.Context, ns, name string) (*sbxv1.Sandbox, error) {
	timeout := s.ReadyTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	poll := s.ReadyPoll
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	deadline := s.now().Add(timeout)

	var sbx sbxv1.Sandbox
	for {
		if err := s.fetchSandbox(ctx, ns, name, &sbx); err != nil {
			// The object was created moments ago, so a NotFound here can only
			// mean the read is looking at something that has not caught up.
			// Keep polling until the deadline rather than reporting that the
			// sandbox does not exist, which would be plainly wrong.
			if !apierrors.IsNotFound(err) || s.now().After(deadline) {
				return nil, err
			}
		}
		switch sbx.Status.Phase {
		case sbxv1.PhaseRunning, sbxv1.PhaseFailed, sbxv1.PhaseExpired:
			return &sbx, nil
		}
		if s.now().After(deadline) {
			return &sbx, nil // still pending; the SDK polls from here
		}
		select {
		case <-ctx.Done():
			return &sbx, nil
		case <-time.After(poll):
		}
	}
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var sbx sbxv1.Sandbox
	err := s.fetchSandbox(r.Context(), p.Namespace, r.PathValue("id"), &sbx)
	if apierrors.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	// A sandbox belonging to another tenant is reported as missing rather than
	// forbidden, so the endpoint cannot be used to discover that it exists.
	if sbx.Labels[sbxv1.LabelTenant] != p.TenantID {
		writeErr(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	resp := toResponse(&sbx)
	// Returned here as well as from create, so a caller that lost the token can
	// reattach to a sandbox it owns. The list endpoint deliberately does not:
	// one credential per response is a smaller thing to leak into a log than
	// every credential the tenant holds.
	resp.Token = sbx.Status.Token
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var list sbxv1.SandboxList
	if err := s.Client.List(r.Context(), &list,
		client.InNamespace(p.Namespace),
		client.MatchingLabels{sbxv1.LabelTenant: p.TenantID}); err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	wantState := r.URL.Query().Get("state")
	out := make([]SandboxResponse, 0, len(list.Items))
	for i := range list.Items {
		resp := toResponse(&list.Items[i])
		if wantState != "" && !strings.EqualFold(resp.State, wantState) {
			continue
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	id := r.PathValue("id")

	var sbx sbxv1.Sandbox
	err := s.fetchSandbox(r.Context(), p.Namespace, id, &sbx)
	if apierrors.IsNotFound(err) || (err == nil && sbx.Labels[sbxv1.LabelTenant] != p.TenantID) {
		writeErr(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	if err := s.Client.Delete(r.Context(), &sbx); err != nil && !apierrors.IsNotFound(err) {
		writeErr(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), AuditEvent{
		TenantID: p.TenantID, Actor: p.KeyID, Action: "sandbox.delete", Target: id,
		IP: clientIP(r), UserAgent: r.UserAgent(), At: s.now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var list sbxv1.SandboxTemplateList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(p.Namespace)); err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	type tplResp struct {
		Name       string `json:"name"`
		Admission  string `json:"admission"`
		Reason     string `json:"admissionReason,omitempty"`
		MinTier    string `json:"minIsolationTier"`
		DefaultTTL int32  `json:"defaultTTLSeconds"`
		MaxTTL     int32  `json:"maxTTLSeconds"`
		HasVolumes bool   `json:"hasVolumes"`
	}
	out := make([]tplResp, 0, len(list.Items))
	for i := range list.Items {
		t := &list.Items[i]
		if !p.Key.AllowsTemplate(t.Name) {
			continue
		}
		out = append(out, tplResp{
			Name: t.Name, Admission: string(t.Status.Admission), Reason: t.Status.AdmissionReason,
			MinTier: string(t.Spec.MinIsolationTier), DefaultTTL: t.Spec.DefaultTTLSeconds,
			MaxTTL: t.Spec.MaxTTLSeconds, HasVolumes: t.Status.HasVolumes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (s *Server) getQuota(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	q, err := s.Store.Quota(r.Context(), p.TenantID)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"unlimited": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "quota_read_failed", err.Error())
		return
	}
	running, _ := s.Store.CountRunning(r.Context(), p.TenantID)
	writeJSON(w, http.StatusOK, map[string]any{
		"maxConcurrent":      q.MaxConcurrent,
		"running":            running,
		"maxCreatePerMinute": q.MaxCreatePerMinute,
	})
}

func toResponse(sbx *sbxv1.Sandbox) SandboxResponse {
	resp := SandboxResponse{
		ID:            sbx.Name,
		State:         strings.ToLower(string(sbx.Status.Phase)),
		Template:      sbx.Spec.TemplateRef,
		IsolationTier: string(sbx.Status.IsolationTier),
		Endpoint:      sbx.Status.Endpoint,
		ColdStart:     sbx.Status.ColdStart,
		Reason:        sbx.Status.Reason,
	}
	if resp.State == "" {
		resp.State = "pending"
	}
	if sbx.Status.ExpiresAt != nil {
		t := sbx.Status.ExpiresAt.Time
		resp.ExpiresAt = &t
	}
	return resp
}

func toCRDFilesystem(in api.FilesystemSpec) sbxv1.FilesystemSpec {
	out := sbxv1.FilesystemSpec{
		Workspace: sbxv1.Workspace{
			Path:      in.Workspace.Path,
			SizeLimit: in.Workspace.SizeLimit,
		},
		AllowSystemPaths: in.AllowSystemPaths,
		Hide:             in.Hide,
	}
	for _, m := range in.Mounts {
		out.Mounts = append(out.Mounts, sbxv1.Mount{
			Path:     m.Path,
			Source:   sbxv1.MountSource{Volume: m.Source.Volume, SubPath: m.Source.SubPath},
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func requestSum(req CreateSandboxRequest) string {
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, api.Error{Code: errCode, Message: msg})
}
