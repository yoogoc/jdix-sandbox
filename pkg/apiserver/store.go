package apiserver

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by every Store lookup that finds nothing.
var ErrNotFound = errors.New("not found")

// Tenant owns namespaces, keys and quota.
type Tenant struct {
	ID        string
	Name      string
	Namespace string
	Status    string
	CreatedAt time.Time
}

// Quota bounds what one tenant can consume.
//
// Without these a single customer's runaway loop takes the cluster down for
// everyone, which is the failure mode that turns one bad afternoon into an
// outage for every tenant.
type Quota struct {
	TenantID             string
	MaxConcurrent        int
	MaxCreatePerMinute   int
	MaxSandboxSecondsDay int64
	MaxIdlePods          int // capacity control, not billing: the platform pays for warm Pods
}

// SandboxRecord mirrors a Sandbox for history and billing.
//
// It is not the source of truth — the Sandbox object is — and it is written
// from watch events, so it is allowed to lag. Treating it as authoritative is
// how two systems end up disagreeing about what is running.
type SandboxRecord struct {
	ID            string
	TenantID      string
	APIKeyID      string
	Template      string
	State         string
	ColdStart     bool
	IsolationTier string
	Node          string
	CreatedAt     time.Time
	ReadyAt       *time.Time
	DeletedAt     *time.Time
	DeleteReason  string
	Metadata      map[string]string
}

// IdempotencyRecord remembers the result of a create so a retry returns the
// same sandbox instead of starting a second one. A network hiccup that doubles
// the bill is a bad way to learn this was missing.
type IdempotencyRecord struct {
	Key        string
	TenantID   string
	RequestSum string
	SandboxID  string
	CreatedAt  time.Time
}

// WarmupRequest is a tenant asking for warm capacity.
//
// Warm Pods are charged to the platform, not the tenant, so a tenant cannot
// simply set a pool size. They ask, and an administrator decides — which is
// also the only thing standing between one tenant and the whole budget.
type WarmupRequest struct {
	ID          string
	TenantID    string
	Template    string
	Replicas    int
	Reason      string
	Status      string // pending | approved | rejected
	RequestedBy string
	ReviewedBy  string
	ReviewNote  string
	CreatedAt   time.Time
	ReviewedAt  *time.Time
}

// PoolBudget is the platform's total warm capacity and how much is spoken for.
type PoolBudget struct {
	TotalIdlePods int
	TotalCPU      string
	TotalMemory   string
	Allocated     int
	UpdatedBy     string
	UpdatedAt     time.Time
}

// Store is the persistence boundary. Everything above it is pure logic, which
// is what lets the whole API be tested without a database.
type Store interface {
	Tenant(ctx context.Context, id string) (*Tenant, error)
	APIKey(ctx context.Context, keyID string) (*APIKey, error)
	TouchAPIKey(ctx context.Context, keyID string, at time.Time) error

	Quota(ctx context.Context, tenantID string) (*Quota, error)
	CountRunning(ctx context.Context, tenantID string) (int, error)
	CountCreatedSince(ctx context.Context, tenantID string, since time.Time) (int, error)

	RecordSandbox(ctx context.Context, r *SandboxRecord) error
	UpdateSandbox(ctx context.Context, r *SandboxRecord) error
	Sandbox(ctx context.Context, id string) (*SandboxRecord, error)
	ListSandboxes(ctx context.Context, tenantID string, limit int, cursor string) ([]*SandboxRecord, string, error)

	// Idempotency returns a previous result for the same key, or ErrNotFound.
	Idempotency(ctx context.Context, tenantID, key string) (*IdempotencyRecord, error)
	SaveIdempotency(ctx context.Context, r *IdempotencyRecord) error

	WarmupRequests(ctx context.Context, status string) ([]*WarmupRequest, error)
	WarmupRequest(ctx context.Context, id string) (*WarmupRequest, error)
	SaveWarmupRequest(ctx context.Context, r *WarmupRequest) error

	PoolBudget(ctx context.Context) (*PoolBudget, error)
	SetPoolBudget(ctx context.Context, b *PoolBudget) error

	Audit(ctx context.Context, e AuditEvent) error
}

// AuditEvent is one line in the audit log.
type AuditEvent struct {
	TenantID  string
	Actor     string
	Action    string
	Target    string
	RequestID string
	IP        string
	UserAgent string
	At        time.Time
	Detail    map[string]string
}
