package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

func TestTenantCanAskForWarmCapacityButNotGrantIt(t *testing.T) {
	f := newFixture(t, ScopeTemplateRead)

	resp := f.do(http.MethodPost, "/v1/templates/py312/warmup-request", f.key,
		WarmupRequestBody{Replicas: 8, Reason: "evaluation runs every morning"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("request returned %d, want 202", resp.StatusCode)
	}
	view := decode[warmupView](t, resp)
	if view.Status != "pending" || view.Replicas != 8 {
		t.Fatalf("view: %+v", view)
	}

	// Asking must not create a pool. That is the entire point of the queue.
	var pool sbxv1.SandboxPool
	err := f.kube.Get(context.Background(), types.NamespacedName{Namespace: tenantNS, Name: "py312"}, &pool)
	if err == nil {
		t.Fatal("a request created a pool without review; a tenant could then spend the platform's budget at will")
	}

	// And a tenant key cannot approve its own request.
	resp = f.do(http.MethodPost, "/v1/admin/warmup-requests/"+view.ID+"/review", f.key,
		ReviewBody{Approve: true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tenant self-approval returned %d, want 403", resp.StatusCode)
	}
}

func TestAdminApprovalCreatesThePool(t *testing.T) {
	f := newFixture(t, ScopeAdmin)

	created := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/templates/py312/warmup-request", f.key, WarmupRequestBody{Replicas: 6}))

	listed := decode[struct {
		Requests []warmupView `json:"requests"`
	}](t, f.do(http.MethodGet, "/v1/admin/warmup-requests", f.key, nil))
	if len(listed.Requests) != 1 || listed.Requests[0].ID != created.ID {
		t.Fatalf("queue: %+v", listed.Requests)
	}

	reviewed := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: true, Replicas: 4, Note: "half for now"}))
	if reviewed.Status != "approved" || reviewed.Replicas != 4 {
		t.Fatalf("review: %+v", reviewed)
	}

	var pool sbxv1.SandboxPool
	if err := f.kube.Get(context.Background(),
		types.NamespacedName{Namespace: tenantNS, Name: "py312"}, &pool); err != nil {
		t.Fatalf("approval did not write a pool: %v", err)
	}
	if pool.Spec.Replicas != 4 {
		t.Fatalf("pool replicas %d, want the approved 4 rather than the requested 6", pool.Spec.Replicas)
	}
	if pool.Spec.NotReadyTimeoutSeconds < 300 {
		t.Error("the default not-ready timeout must be generous, or a CSI attach never finishes in time")
	}
}

func TestApprovalCannotExceedTheBudget(t *testing.T) {
	f := newFixture(t, ScopeAdmin)
	if err := f.store.SetPoolBudget(context.Background(), &PoolBudget{TotalIdlePods: 5}); err != nil {
		t.Fatal(err)
	}

	created := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/templates/py312/warmup-request", f.key, WarmupRequestBody{Replicas: 10}))

	resp := f.do(http.MethodPost, "/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("over-budget approval returned %d, want 409", resp.StatusCode)
	}
	body := decode[map[string]any](t, resp)
	if body["code"] != "budget_exceeded" {
		t.Errorf("error code %v", body["code"])
	}
}

func TestBudgetReportsWhatIsLeft(t *testing.T) {
	f := newFixture(t, ScopeAdmin)
	created := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/templates/py312/warmup-request", f.key, WarmupRequestBody{Replicas: 12}))
	f.do(http.MethodPost, "/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: true}).Body.Close()

	got := decode[map[string]any](t, f.do(http.MethodGet, "/v1/admin/budget", f.key, nil))
	total, alloc, remaining := num(got["totalIdlePods"]), num(got["allocated"]), num(got["remaining"])
	if alloc != 12 {
		t.Fatalf("allocated %d, want 12", alloc)
	}
	if remaining != total-alloc {
		t.Fatalf("remaining %d does not follow from %d - %d", remaining, total, alloc)
	}
}

func TestARequestIsReviewedOnlyOnce(t *testing.T) {
	f := newFixture(t, ScopeAdmin)
	created := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/templates/py312/warmup-request", f.key, WarmupRequestBody{Replicas: 2}))

	f.do(http.MethodPost, "/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: true}).Body.Close()
	resp := f.do(http.MethodPost, "/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second review returned %d, want 409", resp.StatusCode)
	}
}

func TestRejectionLeavesNoPool(t *testing.T) {
	f := newFixture(t, ScopeAdmin)
	created := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/templates/py312/warmup-request", f.key, WarmupRequestBody{Replicas: 3}))

	reviewed := decode[warmupView](t, f.do(http.MethodPost,
		"/v1/admin/warmup-requests/"+created.ID+"/review", f.key,
		ReviewBody{Approve: false, Note: "no budget this quarter"}))
	if reviewed.Status != "rejected" || reviewed.ReviewNote == "" {
		t.Fatalf("review: %+v", reviewed)
	}

	var pool sbxv1.SandboxPool
	if err := f.kube.Get(context.Background(),
		types.NamespacedName{Namespace: tenantNS, Name: "py312"}, &pool); err == nil {
		t.Fatal("a rejected request created a pool anyway")
	}
}

func TestWarmupRequestValidatesItsInput(t *testing.T) {
	f := newFixture(t, ScopeAdmin)

	resp := f.do(http.MethodPost, "/v1/templates/py312/warmup-request", f.key,
		WarmupRequestBody{Replicas: 0})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("zero replicas returned %d, want 400", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/v1/templates/nope/warmup-request", f.key,
		WarmupRequestBody{Replicas: 2})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown template returned %d, want 404", resp.StatusCode)
	}
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}
