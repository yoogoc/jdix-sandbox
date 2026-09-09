package apiserver

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// MemStore is an in-memory Store.
//
// It exists so the API's logic can be tested exhaustively without a database,
// and so a developer can run the whole control plane locally with nothing
// installed. It is not for production: nothing survives a restart.
type MemStore struct {
	mu      sync.RWMutex
	tenants map[string]*Tenant
	keys    map[string]*APIKey
	quotas  map[string]*Quota
	boxes   map[string]*SandboxRecord
	idem    map[string]*IdempotencyRecord
	warmups map[string]*WarmupRequest
	budget  *PoolBudget
	audit   []AuditEvent
}

func NewMemStore() *MemStore {
	return &MemStore{
		tenants: map[string]*Tenant{},
		keys:    map[string]*APIKey{},
		quotas:  map[string]*Quota{},
		boxes:   map[string]*SandboxRecord{},
		idem:    map[string]*IdempotencyRecord{},
		warmups: map[string]*WarmupRequest{},
		// Defaults sized for the reference deployment: roughly 6% of a
		// thousand-sandbox cluster held warm, which is where the latency win
		// stops being worth the standing cost.
		budget: &PoolBudget{TotalIdlePods: 64, TotalCPU: "16", TotalMemory: "32Gi"},
	}
}

// Seed helpers, used by tests and by the local development server.

func (m *MemStore) AddTenant(t *Tenant) { m.mu.Lock(); m.tenants[t.ID] = t; m.mu.Unlock() }
func (m *MemStore) AddKey(k *APIKey)    { m.mu.Lock(); m.keys[k.KeyID] = k; m.mu.Unlock() }
func (m *MemStore) SetQuota(q *Quota)   { m.mu.Lock(); m.quotas[q.TenantID] = q; m.mu.Unlock() }
func (m *MemStore) AuditLog() []AuditEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]AuditEvent(nil), m.audit...)
}

func (m *MemStore) Tenant(_ context.Context, id string) (*Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t, ok := m.tenants[id]; ok {
		return t, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) APIKey(_ context.Context, keyID string) (*APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if k, ok := m.keys[keyID]; ok {
		cp := *k
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) TouchAPIKey(_ context.Context, keyID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[keyID]; ok {
		k.LastUsedAt = &at
	}
	return nil
}

func (m *MemStore) Quota(_ context.Context, tenantID string) (*Quota, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if q, ok := m.quotas[tenantID]; ok {
		return q, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) CountRunning(_ context.Context, tenantID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, b := range m.boxes {
		if b.TenantID == tenantID && b.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) CountCreatedSince(_ context.Context, tenantID string, since time.Time) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, b := range m.boxes {
		if b.TenantID == tenantID && b.CreatedAt.After(since) {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) RecordSandbox(_ context.Context, r *SandboxRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.boxes[r.ID] = &cp
	return nil
}

func (m *MemStore) UpdateSandbox(ctx context.Context, r *SandboxRecord) error {
	return m.RecordSandbox(ctx, r)
}

func (m *MemStore) Sandbox(_ context.Context, id string) (*SandboxRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b, ok := m.boxes[id]; ok {
		cp := *b
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListSandboxes(_ context.Context, tenantID string, limit int, cursor string) ([]*SandboxRecord, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	all := make([]*SandboxRecord, 0, len(m.boxes))
	for _, b := range m.boxes {
		if b.TenantID == tenantID {
			cp := *b
			all = append(all, &cp)
		}
	}
	// Newest first, with the id breaking ties so paging is stable when several
	// sandboxes share a timestamp.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})

	start := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n > 0 {
			start = n
		}
	}
	if start > len(all) {
		start = len(all)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	end := start + limit
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	} else {
		end = len(all)
	}
	return all[start:end], next, nil
}

func (m *MemStore) Idempotency(_ context.Context, tenantID, key string) (*IdempotencyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r, ok := m.idem[tenantID+"/"+key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) SaveIdempotency(_ context.Context, r *IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.idem[r.TenantID+"/"+r.Key] = &cp
	return nil
}

func (m *MemStore) Audit(_ context.Context, e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

func (m *MemStore) WarmupRequests(_ context.Context, status string) ([]*WarmupRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*WarmupRequest, 0, len(m.warmups))
	for _, r := range m.warmups {
		if status != "" && r.Status != status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) WarmupRequest(_ context.Context, id string) (*WarmupRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r, ok := m.warmups[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (m *MemStore) SaveWarmupRequest(_ context.Context, r *WarmupRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.warmups[r.ID] = &cp
	return nil
}

func (m *MemStore) PoolBudget(_ context.Context) (*PoolBudget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := *m.budget
	// Derived, not stored — the same as the Postgres implementation. A counter
	// kept alongside the requests would eventually disagree with them, and the
	// requests are the thing that is actually true.
	cp.Allocated = 0
	for _, r := range m.warmups {
		if r.Status == "approved" {
			cp.Allocated += r.Replicas
		}
	}
	return &cp, nil
}

func (m *MemStore) SetPoolBudget(_ context.Context, b *PoolBudget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *b
	m.budget = &cp
	return nil
}
