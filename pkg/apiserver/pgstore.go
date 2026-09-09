package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the production Store, backed by Postgres.
//
// Every query is parameterised — string concatenation into SQL has no place in
// a service that hands out execution environments — and the read paths that run
// on the create hot path are backed by the partial indexes in schema.sql.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore connects and verifies the connection before returning, so a bad
// DSN fails at start-up rather than on the first request.
func NewPGStore(ctx context.Context, dsn string) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// The API server is stateless and horizontally scaled; a modest per-replica
	// pool keeps the total connection count sane as replicas multiply.
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PGStore{pool: pool}, nil
}

func (s *PGStore) Close() { s.pool.Close() }

// Migrate applies schema.sql. It is idempotent, so running it on every start is
// safe and keeps a fresh environment one command away from working.
func (s *PGStore) Migrate(ctx context.Context, schema string) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgxNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) Tenant(ctx context.Context, id string) (*Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, namespace, status, created_at FROM tenants WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.Namespace, &t.Status, &t.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (s *PGStore) APIKey(ctx context.Context, keyID string) (*APIKey, error) {
	var (
		k      APIKey
		scopes []string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT key_id, tenant_id, name, secret_hash, scopes, allowed_templates,
		       ip_allowlist, expires_at, revoked_at, last_used_at, created_at
		  FROM api_keys WHERE key_id = $1`, keyID).
		Scan(&k.KeyID, &k.TenantID, &k.Name, &k.SecretHash, &scopes, &k.AllowedTemplates,
			&k.IPAllowlist, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	for _, sc := range scopes {
		k.Scopes = append(k.Scopes, Scope(sc))
	}
	return &k, nil
}

func (s *PGStore) TouchAPIKey(ctx context.Context, keyID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = $2 WHERE key_id = $1`, keyID, at)
	return err
}

func (s *PGStore) Quota(ctx context.Context, tenantID string) (*Quota, error) {
	var q Quota
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, max_concurrent, max_create_per_minute,
		       max_sandbox_seconds_day, max_idle_pods
		  FROM quotas WHERE tenant_id = $1`, tenantID).
		Scan(&q.TenantID, &q.MaxConcurrent, &q.MaxCreatePerMinute, &q.MaxSandboxSecondsDay, &q.MaxIdlePods)
	if err != nil {
		return nil, mapErr(err)
	}
	return &q, nil
}

func (s *PGStore) CountRunning(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sandboxes WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&n)
	return n, err
}

func (s *PGStore) CountCreatedSince(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sandboxes WHERE tenant_id = $1 AND created_at > $2`, tenantID, since).Scan(&n)
	return n, err
}

func (s *PGStore) RecordSandbox(ctx context.Context, r *SandboxRecord) error {
	meta, err := json.Marshal(r.Metadata)
	if err != nil {
		return err
	}
	// Upsert rather than insert: this table mirrors watch events, which can
	// arrive out of order or be replayed after a restart.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sandboxes (id, tenant_id, api_key_id, template, state, cold_start,
		                       isolation_tier, node, created_at, ready_at, deleted_at,
		                       delete_reason, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
		    state = EXCLUDED.state,
		    cold_start = EXCLUDED.cold_start,
		    isolation_tier = COALESCE(EXCLUDED.isolation_tier, sandboxes.isolation_tier),
		    node = COALESCE(EXCLUDED.node, sandboxes.node),
		    ready_at = COALESCE(EXCLUDED.ready_at, sandboxes.ready_at),
		    deleted_at = COALESCE(EXCLUDED.deleted_at, sandboxes.deleted_at),
		    delete_reason = COALESCE(NULLIF(EXCLUDED.delete_reason,''), sandboxes.delete_reason)`,
		r.ID, r.TenantID, nullString(r.APIKeyID), r.Template, r.State, r.ColdStart,
		nullString(r.IsolationTier), nullString(r.Node), r.CreatedAt, r.ReadyAt, r.DeletedAt,
		r.DeleteReason, meta)
	return err
}

func (s *PGStore) UpdateSandbox(ctx context.Context, r *SandboxRecord) error {
	return s.RecordSandbox(ctx, r)
}

func (s *PGStore) Sandbox(ctx context.Context, id string) (*SandboxRecord, error) {
	r, err := s.scanSandbox(s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, coalesce(api_key_id,''), template, state, cold_start,
		       coalesce(isolation_tier,''), coalesce(node,''), created_at, ready_at,
		       deleted_at, coalesce(delete_reason,''), metadata
		  FROM sandboxes WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr(err)
	}
	return r, nil
}

func (s *PGStore) ListSandboxes(ctx context.Context, tenantID string, limit int, cursor string) ([]*SandboxRecord, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n > 0 {
			offset = n
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, coalesce(api_key_id,''), template, state, cold_start,
		       coalesce(isolation_tier,''), coalesce(node,''), created_at, ready_at,
		       deleted_at, coalesce(delete_reason,''), metadata
		  FROM sandboxes WHERE tenant_id = $1
		 ORDER BY created_at DESC, id ASC
		 LIMIT $2 OFFSET $3`, tenantID, limit+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []*SandboxRecord
	for rows.Next() {
		r, err := s.scanSandbox(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	// One extra row was fetched purely to learn whether another page exists.
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = strconv.Itoa(offset + limit)
	}
	return out, next, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *PGStore) scanSandbox(row rowScanner) (*SandboxRecord, error) {
	var (
		r    SandboxRecord
		meta []byte
	)
	err := row.Scan(&r.ID, &r.TenantID, &r.APIKeyID, &r.Template, &r.State, &r.ColdStart,
		&r.IsolationTier, &r.Node, &r.CreatedAt, &r.ReadyAt, &r.DeletedAt, &r.DeleteReason, &meta)
	if err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &r.Metadata)
	}
	return &r, nil
}

func (s *PGStore) Idempotency(ctx context.Context, tenantID, key string) (*IdempotencyRecord, error) {
	var r IdempotencyRecord
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, key, request_sum, sandbox_id, created_at
		  FROM idempotency WHERE tenant_id = $1 AND key = $2`, tenantID, key).
		Scan(&r.TenantID, &r.Key, &r.RequestSum, &r.SandboxID, &r.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (s *PGStore) SaveIdempotency(ctx context.Context, r *IdempotencyRecord) error {
	// DO NOTHING on conflict: two concurrent retries must agree on one sandbox,
	// and the first writer decides which.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency (tenant_id, key, request_sum, sandbox_id, created_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (tenant_id, key) DO NOTHING`,
		r.TenantID, r.Key, r.RequestSum, r.SandboxID, r.CreatedAt)
	return err
}

// PurgeIdempotency drops records older than age. Retries arrive within seconds,
// so anything older is dead weight.
func (s *PGStore) PurgeIdempotency(ctx context.Context, age time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency WHERE created_at < $1`, time.Now().Add(-age))
	return err
}

func (s *PGStore) Audit(ctx context.Context, e AuditEvent) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO audit_logs (tenant_id, actor, action, target, request_id, ip, user_agent, at, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		nullString(e.TenantID), nullString(e.Actor), e.Action, nullString(e.Target),
		nullString(e.RequestID), nullString(e.IP), nullString(e.UserAgent), e.At, detail)
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *PGStore) WarmupRequests(ctx context.Context, status string) ([]*WarmupRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, template, replicas, coalesce(reason,''), status,
		       coalesce(requested_by,''), coalesce(reviewed_by,''), coalesce(review_note,''),
		       created_at, reviewed_at
		  FROM warmup_requests
		 WHERE ($1 = '' OR status = $1)
		 ORDER BY created_at ASC`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*WarmupRequest
	for rows.Next() {
		var r WarmupRequest
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Template, &r.Replicas, &r.Reason, &r.Status,
			&r.RequestedBy, &r.ReviewedBy, &r.ReviewNote, &r.CreatedAt, &r.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *PGStore) WarmupRequest(ctx context.Context, id string) (*WarmupRequest, error) {
	var r WarmupRequest
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, template, replicas, coalesce(reason,''), status,
		       coalesce(requested_by,''), coalesce(reviewed_by,''), coalesce(review_note,''),
		       created_at, reviewed_at
		  FROM warmup_requests WHERE id = $1`, id).
		Scan(&r.ID, &r.TenantID, &r.Template, &r.Replicas, &r.Reason, &r.Status,
			&r.RequestedBy, &r.ReviewedBy, &r.ReviewNote, &r.CreatedAt, &r.ReviewedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (s *PGStore) SaveWarmupRequest(ctx context.Context, r *WarmupRequest) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO warmup_requests (id, tenant_id, template, replicas, reason, status,
		                             requested_by, reviewed_by, review_note, created_at, reviewed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (id) DO UPDATE SET
		    status = EXCLUDED.status,
		    reviewed_by = EXCLUDED.reviewed_by,
		    review_note = EXCLUDED.review_note,
		    reviewed_at = EXCLUDED.reviewed_at`,
		r.ID, r.TenantID, r.Template, r.Replicas, nullString(r.Reason), r.Status,
		nullString(r.RequestedBy), nullString(r.ReviewedBy), nullString(r.ReviewNote),
		r.CreatedAt, r.ReviewedAt)
	return err
}

func (s *PGStore) PoolBudget(ctx context.Context) (*PoolBudget, error) {
	var b PoolBudget
	err := s.pool.QueryRow(ctx, `
		SELECT total_idle_pods, coalesce(total_cpu,''), coalesce(total_memory,''),
		       coalesce(updated_by,''), updated_at
		  FROM pool_budget WHERE id = 1`).
		Scan(&b.TotalIdlePods, &b.TotalCPU, &b.TotalMemory, &b.UpdatedBy, &b.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	// Allocated is derived from the pools that were actually approved, so the
	// two numbers cannot drift apart the way a stored counter would.
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(replicas),0) FROM warmup_requests WHERE status = 'approved'`).
		Scan(&b.Allocated); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *PGStore) SetPoolBudget(ctx context.Context, b *PoolBudget) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pool_budget (id, total_idle_pods, total_cpu, total_memory, updated_by, updated_at)
		VALUES (1,$1,$2,$3,$4,now())
		ON CONFLICT (id) DO UPDATE SET
		    total_idle_pods = EXCLUDED.total_idle_pods,
		    total_cpu = EXCLUDED.total_cpu,
		    total_memory = EXCLUDED.total_memory,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = now()`,
		b.TotalIdlePods, nullString(b.TotalCPU), nullString(b.TotalMemory), nullString(b.UpdatedBy))
	return err
}
