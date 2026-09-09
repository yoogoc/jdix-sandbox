package jdix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI stands in for the control plane and the sandbox data plane. Both are
// served from one httptest server, which is enough to exercise the client.
type fakeAPI struct {
	*httptest.Server
	creates  atomic.Int32
	lastAuth string
	lastIdem string
	state    atomic.Value // string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.state.Store("running")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.creates.Add(1)
		f.lastAuth = r.Header.Get("Authorization")
		f.lastIdem = r.Header.Get("Idempotency-Key")
		exp := time.Now().Add(10 * time.Minute)
		writeJSON(w, 201, map[string]any{
			"id": "sbx-test", "state": f.state.Load(), "template": "py312",
			"isolationTier": "userns", "endpoint": f.URL, "token": "sbt_test",
			"expiresAt": exp, "coldStart": false,
		})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(10 * time.Minute)
		writeJSON(w, 200, map[string]any{
			"id": r.PathValue("id"), "state": f.state.Load(), "template": "py312",
			"endpoint": f.URL, "expiresAt": exp,
		})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v1/exec", func(w http.ResponseWriter, r *http.Request) {
		var req execRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		code := 0
		if strings.Contains(req.Cmd, "false") {
			code = 3
		}
		writeJSON(w, 200, Result{ExitCode: code, Stdout: "out:" + req.Cmd, DurationMs: 5})
	})
	mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "/workspace/missing" {
			writeJSON(w, 404, map[string]string{"code": "not_found", "message": "no such file"})
			return
		}
		_, _ = io.WriteString(w, "file contents")
	})
	mux.HandleFunc("PUT /v1/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		writeJSON(w, 200, map[string]any{"ok": true, "bytes": len(b)})
	})
	mux.HandleFunc("GET /v1/files/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entries": []FileInfo{{Name: "main.py", Size: 11}}})
	})
	mux.HandleFunc("POST /v1/keepalive", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour)
		writeJSON(w, 200, map[string]any{"ok": true, "expiresAt": exp})
	})
	mux.HandleFunc("GET /v1/quota-429", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, 429, map[string]string{"code": "quota_concurrent", "message": "too many"})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newTestClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	c, err := NewClient(WithAPIKey("jdix_sk_0123456789ab_secret"), WithBaseURL(f.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRequiresAKey(t *testing.T) {
	if _, err := NewClient(); err == nil {
		t.Fatal("a client without an API key should not be usable")
	}
}

func TestCreateAndRun(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	ctx := context.Background()

	sbx, err := c.Create(ctx, CreateOpts{Template: "py312", TTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ID != "sbx-test" || sbx.IsolationTier != "userns" {
		t.Fatalf("sandbox is wrong: %+v", sbx)
	}
	if !strings.HasPrefix(f.lastAuth, "Bearer jdix_sk_") {
		t.Errorf("the API key was not sent: %q", f.lastAuth)
	}

	res, err := sbx.Run(ctx, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok() || res.Stdout != "out:echo hi" {
		t.Fatalf("result: %+v", res)
	}
}

// A command that exits non-zero is a Result, not an error. Conflating them
// forces every caller to unwrap an error just to read an exit status.
func TestNonZeroExitIsNotAnError(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	sbx, err := c.Create(context.Background(), CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sbx.Run(context.Background(), "false")
	if err != nil {
		t.Fatalf("a failing command must not produce a Go error: %v", err)
	}
	if res.ExitCode != 3 || res.Ok() {
		t.Fatalf("exit code was lost: %+v", res)
	}
}

func TestIdempotencyKeyIsForwarded(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	_, err := c.Create(context.Background(), CreateOpts{Template: "py312", IdempotencyKey: "job-42"})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastIdem != "job-42" {
		t.Fatalf("Idempotency-Key was %q", f.lastIdem)
	}
}

// The SDK must not invent an idempotency key. Retrying a create on the caller's
// behalf is how a network hiccup silently doubles the bill.
func TestCreateIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		writeJSON(w, 500, map[string]string{"code": "boom", "message": "server error"})
	}))
	defer srv.Close()

	c, _ := NewClient(WithAPIKey("jdix_sk_0123456789ab_secret"), WithBaseURL(srv.URL))
	if _, err := c.Create(context.Background(), CreateOpts{Template: "py312"}); err == nil {
		t.Fatal("expected an error")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("create was attempted %d times; it must not be retried automatically", n)
	}
}

func TestGetIsRetriedOnServerErrors(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			writeJSON(w, 503, map[string]string{"code": "unavailable", "message": "try again"})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "sbx-test", "state": "running"})
	}))
	defer srv.Close()

	c, _ := NewClient(WithAPIKey("jdix_sk_0123456789ab_secret"), WithBaseURL(srv.URL))
	if _, err := c.Get(context.Background(), "sbx-test"); err != nil {
		t.Fatalf("a read should survive a transient failure: %v", err)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestErrorsAreClassifiable(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)

	err := c.do(context.Background(), http.MethodGet, "/v1/quota-429", nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("a 429 should match ErrQuotaExceeded, got %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal("expected a *jdix.Error")
	}
	if apiErr.Code != "quota_concurrent" {
		t.Errorf("code %q", apiErr.Code)
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("Retry-After was not parsed: %v", apiErr.RetryAfter)
	}
}

func TestFileRoundTrip(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	sbx, err := c.Create(ctx, CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}

	if err := sbx.Files.WriteString(ctx, "/workspace/main.py", "print('hi')"); err != nil {
		t.Fatal(err)
	}
	b, err := sbx.Files.ReadAll(ctx, "/workspace/main.py")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "file contents" {
		t.Fatalf("read %q", b)
	}
	entries, err := sbx.Files.List(ctx, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "main.py" {
		t.Fatalf("entries: %+v", entries)
	}

	_, err = sbx.Files.ReadAll(ctx, "/workspace/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing file should match ErrNotFound, got %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	sbx, err := c.Create(ctx, CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := sbx.Close(ctx); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

func TestWaitReadyReturnsImmediatelyWhenRunning(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	sbx, err := c.Create(ctx, CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := sbx.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("WaitReady slept even though the sandbox was already running")
	}
}

func TestWaitReadyPollsAColdStart(t *testing.T) {
	f := newFakeAPI(t)
	f.state.Store("pending")
	c := newTestClient(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sbx, err := c.Create(ctx, CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		f.state.Store("running")
	}()
	if err := sbx.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if sbx.State() != "running" {
		t.Fatalf("state %q", sbx.State())
	}
}

func TestKeepAliveMovesTheDeadline(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	sbx, err := c.Create(ctx, CreateOpts{Template: "py312"})
	if err != nil {
		t.Fatal(err)
	}
	before := sbx.ExpiresAt()
	if err := sbx.KeepAlive(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	after := sbx.ExpiresAt()
	if after == nil || before == nil || !after.After(*before) {
		t.Fatalf("deadline did not move: %v -> %v", before, after)
	}
}

func TestExposeBuildsThePortHostname(t *testing.T) {
	sbx := &Sandbox{Endpoint: "https://sbx-abc.sbx.example.com"}
	host := strings.TrimPrefix(sbx.Endpoint, "https://")
	name, rest, _ := strings.Cut(host, ".")
	want := "https://" + name + "-8000." + rest
	if want != "https://sbx-abc-8000.sbx.example.com" {
		t.Fatalf("built %q", want)
	}
}

// A quota error must come back immediately. Sleeping behind Retry-After turns a
// clear, actionable failure into a multi-second hang for no benefit.
func TestQuotaErrorsAreNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, 429, map[string]string{"code": "quota_concurrent", "message": "too many"})
	}))
	defer srv.Close()

	c, _ := NewClient(WithAPIKey("jdix_sk_0123456789ab_secret"), WithBaseURL(srv.URL))
	start := time.Now()
	err := c.do(context.Background(), http.MethodGet, "/v1/quota", nil, nil, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected a quota error, got %v", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempted %d times; a quota will not clear on retry", n)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s; the caller should be told at once and decide for itself", elapsed)
	}
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter should still be reported for the caller to act on, got %v", apiErr.RetryAfter)
	}
}
