package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sbxv1 "jdix.io/sandbox/pkg/apis/sandbox/v1alpha1"
)

const (
	tenantID = "t_abc"
	tenantNS = "tenant-abc"
)

type fixture struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	store *MemStore
	kube  client.Client
	key   string
}

func newFixture(t *testing.T, scopes ...Scope) *fixture {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []Scope{ScopeSandboxCreate, ScopeSandboxRead, ScopeSandboxDelete, ScopeTemplateRead}
	}

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := sbxv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tpl := &sbxv1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "py312", Namespace: tenantNS},
		Spec:       sbxv1.SandboxTemplateSpec{MinIsolationTier: sbxv1.TierUserns, DefaultTTLSeconds: 600},
		Status:     sbxv1.SandboxTemplateStatus{Admission: sbxv1.AdmissionApproved},
	}
	kube := fake.NewClientBuilder().WithScheme(s).WithObjects(tpl).
		WithStatusSubresource(&sbxv1.Sandbox{}, &sbxv1.SandboxTemplate{}).Build()

	store := NewMemStore()
	store.AddTenant(&Tenant{ID: tenantID, Name: "abc", Namespace: tenantNS, Status: "active"})

	gen, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store.AddKey(&APIKey{
		KeyID: gen.KeyID, TenantID: tenantID, Name: "test",
		SecretHash: gen.Hash, Scopes: scopes, CreatedAt: time.Now(),
	})

	srv := &Server{
		Client:       kube,
		Store:        store,
		Auth:         NewAuthenticator(store),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadyTimeout: 300 * time.Millisecond,
		ReadyPoll:    10 * time.Millisecond,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, srv: srv, http: ts, store: store, kube: kube, key: gen.Plaintext}
}

func (f *fixture) do(method, path, key string, body any) *http.Response {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.http.URL+path, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

func TestAPIKeyRoundTrip(t *testing.T) {
	gen, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyID, secret, err := ParseKey(gen.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != gen.KeyID {
		t.Fatalf("key id round trip: %q vs %q", keyID, gen.KeyID)
	}
	if !VerifySecret(secret, gen.Hash) {
		t.Fatal("the generated secret does not verify against its own hash")
	}
	if VerifySecret(secret+"x", gen.Hash) {
		t.Fatal("a wrong secret verified")
	}
	// The plaintext must never be recoverable from what is stored.
	if bytes.Contains([]byte(gen.Hash), []byte(secret)) {
		t.Fatal("the stored hash contains the secret")
	}
}

func TestParseKeyRejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{
		"", "nope", "jdix_sk_", "jdix_sk_short_secret", "Bearer jdix_sk_x_y",
		"jdix_sk_0123456789ab", "jdix_sk_0123456789ab_",
	} {
		if _, _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted a malformed key", bad)
		}
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{"", "jdix_sk_0123456789ab_wrongsecret", "garbage"} {
		resp := f.do(http.MethodGet, "/v1/sandboxes", key, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q got %d, want 401", key, resp.StatusCode)
		}
	}
}

func TestScopesAreEnforced(t *testing.T) {
	f := newFixture(t, ScopeSandboxRead) // read only
	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create with a read-only key returned %d, want 403", resp.StatusCode)
	}
	body := decode[map[string]any](t, resp)
	if body["code"] != "insufficient_scope" {
		t.Errorf("error should name the missing scope: %v", body)
	}
}

func TestCreateSandboxProducesAnObject(t *testing.T) {
	f := newFixture(t)
	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key,
		CreateSandboxRequest{Template: "py312", TTLSeconds: 600, Env: map[string]string{"RUN_ID": "42"}})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create returned %d: %s", resp.StatusCode, b)
	}
	got := decode[SandboxResponse](t, resp)
	if got.ID == "" || got.Template != "py312" {
		t.Fatalf("response is wrong: %+v", got)
	}
	// The controller has not run, so it is still pending — and saying so beats
	// holding the connection open for a cold start.
	if got.State != "pending" {
		t.Errorf("state %q, want pending", got.State)
	}

	var sbx sbxv1.Sandbox
	if err := f.kube.Get(context.Background(), types.NamespacedName{Namespace: tenantNS, Name: got.ID}, &sbx); err != nil {
		t.Fatalf("the Sandbox object was not created: %v", err)
	}
	if sbx.Labels[sbxv1.LabelTenant] != tenantID {
		t.Error("the Sandbox is not stamped with its tenant, so tenant filtering would leak")
	}
	if len(sbx.Spec.Env) != 1 || sbx.Spec.Env[0].Name != "RUN_ID" {
		t.Errorf("env not carried through: %+v", sbx.Spec.Env)
	}
}

func TestCreateWaitsForTheControllerToFinish(t *testing.T) {
	f := newFixture(t)
	f.srv.ReadyTimeout = 3 * time.Second

	// Simulate the controller reaching Running shortly after the create.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var list sbxv1.SandboxList
			if err := f.kube.List(context.Background(), &list, client.InNamespace(tenantNS)); err == nil {
				for i := range list.Items {
					s := &list.Items[i]
					if s.Status.Phase == "" {
						exp := metav1.NewTime(time.Now().Add(10 * time.Minute))
						s.Status.Phase = sbxv1.PhaseRunning
						s.Status.Endpoint = "https://" + s.Name + ".sbx.example.com"
						s.Status.Token = "sbt_from_the_controller"
						s.Status.IsolationTier = sbxv1.TierUserns
						s.Status.ExpiresAt = &exp
						_ = f.kube.Status().Update(context.Background(), s)
						return
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	got := decode[SandboxResponse](t, resp)
	if got.State != "running" {
		t.Fatalf("state %q, want running", got.State)
	}
	if got.Endpoint == "" || got.ExpiresAt == nil || got.IsolationTier != string(sbxv1.TierUserns) {
		t.Fatalf("a ready sandbox must carry everything the SDK needs: %+v", got)
	}
	if got.Token != "sbt_from_the_controller" {
		t.Fatalf("token %q: without it the endpoint above is unusable and every data-plane call is a 401", got.Token)
	}
}

// The token is a credential, so where it appears is a decision rather than an
// accident: on the two single-sandbox reads, and not in a bulk listing.
func TestTokenIsReturnedByGetButNotByList(t *testing.T) {
	f := newFixture(t)
	f.srv.ReadyTimeout = time.Millisecond

	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	id := decode[SandboxResponse](t, resp).ID

	var sbx sbxv1.Sandbox
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: tenantNS, Name: id}, &sbx); err != nil {
		t.Fatal(err)
	}
	sbx.Status.Phase = sbxv1.PhaseRunning
	sbx.Status.Token = "sbt_from_the_controller"
	if err := f.kube.Status().Update(context.Background(), &sbx); err != nil {
		t.Fatal(err)
	}

	one := decode[SandboxResponse](t, f.do(http.MethodGet, "/v1/sandboxes/"+id, f.key, nil))
	if one.Token != "sbt_from_the_controller" {
		t.Errorf("get returned token %q; a caller that lost it cannot reattach", one.Token)
	}

	var listed struct {
		Sandboxes []SandboxResponse `json:"sandboxes"`
	}
	body := f.do(http.MethodGet, "/v1/sandboxes", f.key, nil)
	defer body.Body.Close()
	if err := json.NewDecoder(body.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sandboxes) != 1 {
		t.Fatalf("listed %d sandboxes", len(listed.Sandboxes))
	}
	if listed.Sandboxes[0].Token != "" {
		t.Errorf("list leaked a credential: %q", listed.Sandboxes[0].Token)
	}
}

// A retried create must return the first sandbox, not start a second one: a
// network hiccup that doubles the bill is a bad way to discover this is missing.
func TestIdempotentCreateReturnsTheSameSandbox(t *testing.T) {
	f := newFixture(t)
	body := CreateSandboxRequest{Template: "py312", TTLSeconds: 600}

	req1, _ := http.NewRequest(http.MethodPost, f.http.URL+"/v1/sandboxes", jsonBody(body))
	req1.Header.Set("Authorization", "Bearer "+f.key)
	req1.Header.Set("Idempotency-Key", "abc-123")
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	first := decode[SandboxResponse](t, resp1)

	req2, _ := http.NewRequest(http.MethodPost, f.http.URL+"/v1/sandboxes", jsonBody(body))
	req2.Header.Set("Authorization", "Bearer "+f.key)
	req2.Header.Set("Idempotency-Key", "abc-123")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	second := decode[SandboxResponse](t, resp2)

	if first.ID != second.ID {
		t.Fatalf("retry created a second sandbox: %s then %s", first.ID, second.ID)
	}
	var list sbxv1.SandboxList
	if err := f.kube.List(context.Background(), &list, client.InNamespace(tenantNS)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d Sandbox objects exist, want 1", len(list.Items))
	}
}

func TestIdempotencyKeyReusedWithADifferentBodyIsRejected(t *testing.T) {
	f := newFixture(t)
	send := func(body CreateSandboxRequest) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/v1/sandboxes", jsonBody(body))
		req.Header.Set("Authorization", "Bearer "+f.key)
		req.Header.Set("Idempotency-Key", "same-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	send(CreateSandboxRequest{Template: "py312", TTLSeconds: 600}).Body.Close()
	resp := send(CreateSandboxRequest{Template: "py312", TTLSeconds: 999})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reusing a key with a different body returned %d, want 409", resp.StatusCode)
	}
}

func TestConcurrencyQuotaIsEnforced(t *testing.T) {
	f := newFixture(t)
	f.store.SetQuota(&Quota{TenantID: tenantID, MaxConcurrent: 2})

	for i := 0; i < 2; i++ {
		resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("create %d returned %d: %s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}

	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-quota create returned %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves clients guessing, and they guess badly")
	}
	body := decode[map[string]any](t, resp)
	if body["code"] != "quota_concurrent" {
		t.Errorf("error code %v", body["code"])
	}
}

func TestCreateRateQuotaIsEnforced(t *testing.T) {
	f := newFixture(t)
	f.store.SetQuota(&Quota{TenantID: tenantID, MaxCreatePerMinute: 1})

	f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"}).Body.Close()
	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second create returned %d, want 429", resp.StatusCode)
	}
}

func TestKeyRestrictedToOtherTemplatesIsRefused(t *testing.T) {
	f := newFixture(t)
	key, _ := f.store.APIKey(context.Background(), keyIDOf(t, f.key))
	key.AllowedTemplates = []string{"node20"}
	f.store.AddKey(key)
	f.srv.Auth.Invalidate(key.KeyID)

	resp := f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("returned %d, want 403", resp.StatusCode)
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	f := newFixture(t)
	key, _ := f.store.APIKey(context.Background(), keyIDOf(t, f.key))
	now := time.Now()
	key.RevokedAt = &now
	f.store.AddKey(key)
	f.srv.Auth.Invalidate(key.KeyID) // revocation is immediate once the cache is cleared

	resp := f.do(http.MethodGet, "/v1/sandboxes", f.key, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked key returned %d, want 401", resp.StatusCode)
	}
}

func TestExpiredKeyStopsWorking(t *testing.T) {
	f := newFixture(t)
	key, _ := f.store.APIKey(context.Background(), keyIDOf(t, f.key))
	past := time.Now().Add(-time.Hour)
	key.ExpiresAt = &past
	f.store.AddKey(key)
	f.srv.Auth.Invalidate(key.KeyID)

	resp := f.do(http.MethodGet, "/v1/sandboxes", f.key, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired key returned %d, want 401", resp.StatusCode)
	}
}

// Another tenant's sandbox is reported as missing rather than forbidden, so the
// endpoint cannot be used to find out that it exists.
func TestOtherTenantsSandboxesAreInvisible(t *testing.T) {
	f := newFixture(t)
	other := &sbxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbx-other", Namespace: tenantNS,
			Labels: map[string]string{sbxv1.LabelTenant: "t_other"},
		},
		Spec: sbxv1.SandboxSpec{TemplateRef: "py312"},
	}
	if err := f.kube.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	resp := f.do(http.MethodGet, "/v1/sandboxes/sbx-other", f.key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("returned %d, want 404", resp.StatusCode)
	}
	resp = f.do(http.MethodDelete, "/v1/sandboxes/sbx-other", f.key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete returned %d, want 404", resp.StatusCode)
	}

	listResp := f.do(http.MethodGet, "/v1/sandboxes", f.key, nil)
	list := decode[struct {
		Sandboxes []SandboxResponse `json:"sandboxes"`
	}](t, listResp)
	for _, s := range list.Sandboxes {
		if s.ID == "sbx-other" {
			t.Fatal("another tenant's sandbox appeared in the list")
		}
	}
}

func TestDeleteRemovesTheSandbox(t *testing.T) {
	f := newFixture(t)
	created := decode[SandboxResponse](t,
		f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"}))

	resp := f.do(http.MethodDelete, "/v1/sandboxes/"+created.ID, f.key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete returned %d", resp.StatusCode)
	}
	var sbx sbxv1.Sandbox
	err := f.kube.Get(context.Background(), types.NamespacedName{Namespace: tenantNS, Name: created.ID}, &sbx)
	if err == nil && sbx.DeletionTimestamp == nil {
		t.Fatal("the Sandbox was not deleted")
	}
}

func TestAuditLogRecordsCreatesAndDeletes(t *testing.T) {
	f := newFixture(t)
	created := decode[SandboxResponse](t,
		f.do(http.MethodPost, "/v1/sandboxes", f.key, CreateSandboxRequest{Template: "py312"}))
	f.do(http.MethodDelete, "/v1/sandboxes/"+created.ID, f.key, nil).Body.Close()

	actions := map[string]bool{}
	for _, e := range f.store.AuditLog() {
		actions[e.Action] = true
		if e.TenantID != tenantID || e.Actor == "" {
			t.Errorf("audit entry is missing attribution: %+v", e)
		}
	}
	for _, want := range []string{"sandbox.create", "sandbox.delete"} {
		if !actions[want] {
			t.Errorf("no audit entry for %s", want)
		}
	}
}

func TestIPAllowlist(t *testing.T) {
	cases := []struct {
		list []string
		ip   string
		want bool
	}{
		{nil, "1.2.3.4", true},
		{[]string{"1.2.3.4"}, "1.2.3.4", true},
		{[]string{"1.2.3.4"}, "1.2.3.5", false},
		{[]string{"10.0.0.0/8"}, "10.1.2.3", true},
		{[]string{"10.0.0.0/8"}, "192.168.1.1", false},
		{[]string{"bogus"}, "1.2.3.4", false},
	}
	for _, tc := range cases {
		if got := ipAllowed(tc.list, tc.ip); got != tc.want {
			t.Errorf("ipAllowed(%v, %q) = %v, want %v", tc.list, tc.ip, got, tc.want)
		}
	}
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func keyIDOf(t *testing.T, plaintext string) string {
	t.Helper()
	id, _, err := ParseKey(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
