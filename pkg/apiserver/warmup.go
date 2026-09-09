package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

// WarmupRequestBody is a tenant asking for warm capacity.
type WarmupRequestBody struct {
	Replicas int    `json:"replicas"`
	Reason   string `json:"reason"`
}

// warmupView is what both tenants and administrators see.
type warmupView struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenantId"`
	Template   string     `json:"template"`
	Replicas   int        `json:"replicas"`
	Reason     string     `json:"reason,omitempty"`
	Status     string     `json:"status"`
	ReviewedBy string     `json:"reviewedBy,omitempty"`
	ReviewNote string     `json:"reviewNote,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ReviewedAt *time.Time `json:"reviewedAt,omitempty"`
}

func viewOf(r *WarmupRequest) warmupView {
	return warmupView{
		ID: r.ID, TenantID: r.TenantID, Template: r.Template, Replicas: r.Replicas,
		Reason: r.Reason, Status: r.Status, ReviewedBy: r.ReviewedBy,
		ReviewNote: r.ReviewNote, CreatedAt: r.CreatedAt, ReviewedAt: r.ReviewedAt,
	}
}

// requestWarmup records a tenant's ask. It does not create a pool: that is the
// whole point of the queue.
func (s *Server) requestWarmup(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	template := r.PathValue("name")

	var body WarmupRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.Replicas <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "replicas must be greater than zero")
		return
	}
	if !p.Key.AllowsTemplate(template) {
		writeErr(w, http.StatusForbidden, "template_not_allowed",
			"this API key is restricted to a different set of templates")
		return
	}

	var tpl sbxv1.SandboxTemplate
	if err := s.Client.Get(r.Context(),
		client.ObjectKey{Namespace: p.Namespace, Name: template}, &tpl); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "template_not_found", "no such template")
			return
		}
		writeErr(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}

	req := &WarmupRequest{
		ID: "wr-" + randomSuffix(), TenantID: p.TenantID, Template: template,
		Replicas: body.Replicas, Reason: body.Reason, Status: "pending",
		RequestedBy: p.KeyID, CreatedAt: s.now(),
	}
	if err := s.Store.SaveWarmupRequest(r.Context(), req); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), AuditEvent{
		TenantID: p.TenantID, Actor: p.KeyID, Action: "warmup.request", Target: template,
		IP: clientIP(r), UserAgent: r.UserAgent(), At: s.now(),
		Detail: map[string]string{"replicas": fmt.Sprint(body.Replicas)},
	})
	writeJSON(w, http.StatusAccepted, viewOf(req))
}

// listWarmupRequests shows an administrator the queue.
func (s *Server) listWarmupRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	reqs, err := s.Store.WarmupRequests(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	out := make([]warmupView, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, viewOf(req))
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// ReviewBody is an administrator's decision.
type ReviewBody struct {
	Approve  bool   `json:"approve"`
	Replicas int    `json:"replicas,omitempty"` // may differ from what was asked
	Note     string `json:"note,omitempty"`
}

// reviewWarmup approves or rejects a request, and on approval writes the pool.
//
// The budget is checked here rather than trusted from a stored counter: the
// number that matters is what is actually allocated, and deriving it means the
// two can never disagree.
func (s *Server) reviewWarmup(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())

	var body ReviewBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	req, err := s.Store.WarmupRequest(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "no such warm-up request")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if req.Status != "pending" {
		writeErr(w, http.StatusConflict, "already_reviewed",
			"this request was already "+req.Status)
		return
	}

	replicas := body.Replicas
	if replicas <= 0 {
		replicas = req.Replicas
	}

	now := s.now()
	req.ReviewedBy, req.ReviewNote, req.ReviewedAt = p.KeyID, body.Note, &now

	if !body.Approve {
		req.Status = "rejected"
		if err := s.Store.SaveWarmupRequest(r.Context(), req); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, viewOf(req))
		return
	}

	budget, err := s.Store.PoolBudget(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if remaining := budget.TotalIdlePods - budget.Allocated; replicas > remaining {
		writeErr(w, http.StatusConflict, "budget_exceeded", fmt.Sprintf(
			"approving %d warm Pods would exceed the platform budget: %d of %d are already allocated",
			replicas, budget.Allocated, budget.TotalIdlePods))
		return
	}

	if err := s.upsertPool(r.Context(), req, replicas); err != nil {
		writeErr(w, http.StatusInternalServerError, "pool_write_failed", err.Error())
		return
	}

	req.Status, req.Replicas = "approved", replicas
	if err := s.Store.SaveWarmupRequest(r.Context(), req); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), AuditEvent{
		TenantID: req.TenantID, Actor: p.KeyID, Action: "warmup.approve", Target: req.Template,
		IP: clientIP(r), UserAgent: r.UserAgent(), At: now,
		Detail: map[string]string{"replicas": fmt.Sprint(replicas)},
	})
	writeJSON(w, http.StatusOK, viewOf(req))
}

// upsertPool writes the SandboxPool an approval authorises.
func (s *Server) upsertPool(ctx context.Context, req *WarmupRequest, replicas int) error {
	var tenant *Tenant
	t, err := s.Store.Tenant(ctx, req.TenantID)
	if err != nil {
		return err
	}
	tenant = t

	var pool sbxv1.SandboxPool
	key := client.ObjectKey{Namespace: tenant.Namespace, Name: req.Template}
	err = s.Client.Get(ctx, key, &pool)
	if apierrors.IsNotFound(err) {
		pool = sbxv1.SandboxPool{
			ObjectMeta: metav1.ObjectMeta{Name: req.Template, Namespace: tenant.Namespace},
			Spec: sbxv1.SandboxPoolSpec{
				TemplateRef: req.Template,
				Replicas:    int32(replicas),
				MinReplicas: 1,
				MaxReplicas: int32(replicas) * 3,
				MaxSurge:    3,
				// Generous by default. A timeout shorter than a CSI attach
				// deletes Pods that were about to become ready, and the pool
				// then rebuilds forever without ever filling.
				NotReadyTimeoutSeconds: 300,
				IdleTTLSeconds:         3600,
			},
		}
		return s.Client.Create(ctx, &pool)
	}
	if err != nil {
		return err
	}
	pool.Spec.Replicas = int32(replicas)
	if pool.Spec.MaxReplicas < int32(replicas) {
		pool.Spec.MaxReplicas = int32(replicas) * 3
	}
	return s.Client.Update(ctx, &pool)
}

// getBudget reports the platform's warm capacity and what is left of it.
func (s *Server) getBudget(w http.ResponseWriter, r *http.Request) {
	b, err := s.Store.PoolBudget(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totalIdlePods": b.TotalIdlePods,
		"allocated":     b.Allocated,
		"remaining":     b.TotalIdlePods - b.Allocated,
		"totalCPU":      b.TotalCPU,
		"totalMemory":   b.TotalMemory,
	})
}
