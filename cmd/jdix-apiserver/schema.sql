-- jdix-sandbox control-plane schema.
--
-- The Kubernetes objects are the source of truth for what is running. This
-- database holds what Kubernetes should not: tenants, credentials, quota,
-- usage and the audit trail. The `sandboxes` table is a mirror written from
-- watch events, kept for history and billing — never read to decide anything.

CREATE TABLE IF NOT EXISTS tenants (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    namespace   TEXT NOT NULL UNIQUE,
    status      TEXT NOT NULL DEFAULT 'active',
    oidc_domain TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    oidc_sub   TEXT NOT NULL UNIQUE,
    email      TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'developer',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- key_id is stored in the clear and indexed; the secret only ever exists as an
-- argon2id verifier. A stolen dump is therefore not a stolen fleet.
CREATE TABLE IF NOT EXISTS api_keys (
    key_id            TEXT PRIMARY KEY,
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    secret_hash       TEXT NOT NULL,
    scopes            TEXT[] NOT NULL DEFAULT '{}',
    allowed_templates TEXT[] NOT NULL DEFAULT '{}',
    ip_allowlist      TEXT[] NOT NULL DEFAULT '{}',
    expires_at        TIMESTAMPTZ,
    revoked_at        TIMESTAMPTZ,
    last_used_at      TIMESTAMPTZ,
    created_by        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_keys_tenant_idx ON api_keys (tenant_id);

CREATE TABLE IF NOT EXISTS quotas (
    tenant_id                TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    max_concurrent           INT NOT NULL DEFAULT 0,
    max_create_per_minute    INT NOT NULL DEFAULT 0,
    max_sandbox_seconds_day  BIGINT NOT NULL DEFAULT 0,
    -- Capacity control, not billing: warm Pods are charged to the platform, so
    -- this caps how much an administrator may allocate to one tenant.
    max_idle_pods            INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    api_key_id     TEXT,
    template       TEXT NOT NULL,
    state          TEXT NOT NULL,
    cold_start     BOOLEAN NOT NULL DEFAULT FALSE,
    isolation_tier TEXT,
    node           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at       TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    delete_reason  TEXT,
    metadata       JSONB NOT NULL DEFAULT '{}'
);
-- The two questions asked on every create: how many are running, and how many
-- were started in the last minute.
CREATE INDEX IF NOT EXISTS sandboxes_running_idx ON sandboxes (tenant_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sandboxes_created_idx ON sandboxes (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS idempotency (
    tenant_id   TEXT NOT NULL,
    key         TEXT NOT NULL,
    request_sum TEXT NOT NULL,
    sandbox_id  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
-- Retries arrive within seconds; keeping these forever would grow without limit.
CREATE INDEX IF NOT EXISTS idempotency_expiry_idx ON idempotency (created_at);

CREATE TABLE IF NOT EXISTS usage_records (
    id               BIGSERIAL PRIMARY KEY,
    sandbox_id       TEXT NOT NULL,
    tenant_id        TEXT NOT NULL,
    sandbox_seconds  BIGINT NOT NULL DEFAULT 0,
    cpu_seconds      DOUBLE PRECISION NOT NULL DEFAULT 0,
    mem_gb_seconds   DOUBLE PRECISION NOT NULL DEFAULT 0,
    egress_bytes     BIGINT NOT NULL DEFAULT 0,
    -- Internal cost attribution: which tenant's warm pool consumed which share
    -- of the platform's budget. Not billed to the tenant today.
    idle_pod_seconds BIGINT NOT NULL DEFAULT 0,
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS usage_tenant_idx ON usage_records (tenant_id, recorded_at DESC);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT,
    actor      TEXT,
    action     TEXT NOT NULL,
    target     TEXT,
    request_id TEXT,
    ip         INET,
    user_agent TEXT,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    detail     JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS audit_tenant_idx ON audit_logs (tenant_id, at DESC);

-- A tenant asking for warm capacity, and an administrator's answer.
--
-- Warm Pods are charged to the platform, so pool size is not a tenant-writable
-- field. They ask here; someone with the budget decides.
CREATE TABLE IF NOT EXISTS warmup_requests (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    template     TEXT NOT NULL,
    replicas     INT NOT NULL,
    reason       TEXT,
    status       TEXT NOT NULL DEFAULT 'pending',
    requested_by TEXT,
    reviewed_by  TEXT,
    review_note  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS warmup_pending_idx ON warmup_requests (status, created_at);

-- A single row holding the platform's total warm capacity. How much of it is
-- spoken for is derived from the approved requests rather than stored, so the
-- two numbers cannot drift apart.
CREATE TABLE IF NOT EXISTS pool_budget (
    id              INT PRIMARY KEY DEFAULT 1,
    total_idle_pods INT NOT NULL DEFAULT 64,
    total_cpu       TEXT NOT NULL DEFAULT '16',
    total_memory    TEXT NOT NULL DEFAULT '32Gi',
    updated_by      TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pool_budget_single_row CHECK (id = 1)
);
INSERT INTO pool_budget (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
