package apiserver

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Two Store implementations exist and they have already drifted once: the
// in-memory one stored the allocated budget while Postgres derived it, so the
// same sequence of calls produced different answers. These tests state the
// semantics both must satisfy, and run against whichever backends are
// available.
func TestStoreConformance(t *testing.T) {
	backends := map[string]func(t *testing.T) Store{
		"mem": func(t *testing.T) Store { return NewMemStore() },
	}
	// Opt in with DATABASE_URL; the schema is applied to whatever it points at,
	// so point it at a throwaway database.
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		backends["postgres"] = func(t *testing.T) Store {
			pg, err := NewPGStore(context.Background(), dsn)
			if err != nil {
				t.Skipf("postgres unavailable: %v", err)
			}
			t.Cleanup(pg.Close)
			schema, err := os.ReadFile("../../config/db/schema.sql")
			if err != nil {
				t.Fatal(err)
			}
			if err := pg.Migrate(context.Background(), string(schema)); err != nil {
				t.Fatal(err)
			}
			return pg
		}
	}

	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			runStoreConformance(t, open(t))
		})
	}
}

func runStoreConformance(t *testing.T, s Store) {
	ctx := context.Background()
	seedTenant(t, s, "t_conf", "tenant-conf")

	t.Run("missing lookups report ErrNotFound", func(t *testing.T) {
		if _, err := s.Tenant(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Tenant: %v", err)
		}
		if _, err := s.APIKey(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("APIKey: %v", err)
		}
		if _, err := s.Sandbox(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Sandbox: %v", err)
		}
		if _, err := s.Idempotency(ctx, "t_conf", "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Idempotency: %v", err)
		}
		if _, err := s.WarmupRequest(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("WarmupRequest: %v", err)
		}
	})

	t.Run("recording a sandbox twice updates rather than duplicates", func(t *testing.T) {
		rec := &SandboxRecord{
			ID: "sbx-conf", TenantID: "t_conf", Template: "py312",
			State: "pending", CreatedAt: time.Now(),
		}
		if err := s.RecordSandbox(ctx, rec); err != nil {
			t.Fatal(err)
		}
		rec.State = "running"
		if err := s.UpdateSandbox(ctx, rec); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sandbox(ctx, "sbx-conf")
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			t.Errorf("state %q, want running", got.State)
		}
		n, err := s.CountRunning(ctx, "t_conf")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("CountRunning = %d, want 1: a second write must not double-count", n)
		}
	})

	t.Run("deleted sandboxes stop counting as running", func(t *testing.T) {
		now := time.Now()
		if err := s.UpdateSandbox(ctx, &SandboxRecord{
			ID: "sbx-conf", TenantID: "t_conf", Template: "py312", State: "expired",
			CreatedAt: now.Add(-time.Hour), DeletedAt: &now,
		}); err != nil {
			t.Fatal(err)
		}
		n, _ := s.CountRunning(ctx, "t_conf")
		if n != 0 {
			t.Errorf("CountRunning = %d after deletion, want 0", n)
		}
	})

	t.Run("idempotency records are write-once", func(t *testing.T) {
		first := &IdempotencyRecord{
			Key: "k1", TenantID: "t_conf", RequestSum: "sum", SandboxID: "sbx-first",
			CreatedAt: time.Now(),
		}
		if err := s.SaveIdempotency(ctx, first); err != nil {
			t.Fatal(err)
		}
		got, err := s.Idempotency(ctx, "t_conf", "k1")
		if err != nil {
			t.Fatal(err)
		}
		if got.SandboxID != "sbx-first" {
			t.Fatalf("sandbox %q", got.SandboxID)
		}
	})

	t.Run("allocated budget is derived from approved requests", func(t *testing.T) {
		before, err := s.PoolBudget(ctx)
		if err != nil {
			t.Fatal(err)
		}

		pending := &WarmupRequest{
			ID: "wr-pending", TenantID: "t_conf", Template: "py312",
			Replicas: 5, Status: "pending", CreatedAt: time.Now(),
		}
		approved := &WarmupRequest{
			ID: "wr-approved", TenantID: "t_conf", Template: "py312",
			Replicas: 7, Status: "approved", CreatedAt: time.Now(),
		}
		for _, r := range []*WarmupRequest{pending, approved} {
			if err := s.SaveWarmupRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
		}

		after, err := s.PoolBudget(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Only the approved request counts, and it counts exactly once.
		if got := after.Allocated - before.Allocated; got != 7 {
			t.Fatalf("allocated moved by %d, want 7 (pending requests must not reserve budget)", got)
		}
	})

	t.Run("the queue filters by status", func(t *testing.T) {
		pending, err := s.WarmupRequests(ctx, "pending")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pending {
			if r.Status != "pending" {
				t.Fatalf("status filter leaked a %q request", r.Status)
			}
		}
		all, err := s.WarmupRequests(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < len(pending) {
			t.Fatal("an empty filter should return at least as many as a specific one")
		}
	})

	t.Run("audit writes are accepted", func(t *testing.T) {
		if err := s.Audit(ctx, AuditEvent{
			TenantID: "t_conf", Actor: "key", Action: "test", At: time.Now(),
			Detail: map[string]string{"k": "v"},
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// seedTenant inserts a tenant through whichever door the backend offers.
func seedTenant(t *testing.T, s Store, id, ns string) {
	t.Helper()
	tenant := &Tenant{ID: id, Name: id, Namespace: ns, Status: "active", CreatedAt: time.Now()}
	switch impl := s.(type) {
	case *MemStore:
		impl.AddTenant(tenant)
	case *PGStore:
		if _, err := impl.pool.Exec(context.Background(), `
			INSERT INTO tenants (id, name, namespace, status)
			VALUES ($1,$2,$3,'active') ON CONFLICT (id) DO NOTHING`, id, id, ns); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no seeding path for %T", s)
	}
}
