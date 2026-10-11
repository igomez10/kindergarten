-- Platform metadata schema: who exists, what they asked for, and what the
-- reconciler has observed. See postgresql-cloud-platform-implementation-plan.md
-- §2.2–2.3.

CREATE TABLE users (
    id         UUID PRIMARY KEY,
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (issuer, subject)
);

-- One row per tenant. The quota columns double as the row the API locks
-- (SELECT ... FOR UPDATE) while it checks and reserves capacity, so concurrent
-- creates in one organization are serialized.
CREATE TABLE organizations (
    id               UUID PRIMARY KEY,
    name             TEXT NOT NULL,
    k8s_namespace    TEXT NOT NULL UNIQUE,
    max_instances    INTEGER NOT NULL,
    max_cpu_millis   INTEGER NOT NULL,
    max_memory_mib   INTEGER NOT NULL,
    max_storage_gib  INTEGER NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE organization_members (
    organization_id UUID NOT NULL REFERENCES organizations(id),
    user_id         UUID NOT NULL REFERENCES users(id),
    role            TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (organization_id, user_id)
);

CREATE INDEX organization_members_user_idx ON organization_members (user_id);

CREATE TABLE instances (
    id UUID PRIMARY KEY,

    organization_id UUID NOT NULL REFERENCES organizations(id),

    name TEXT NOT NULL,

    service_type TEXT NOT NULL CHECK (service_type = 'postgres'),

    desired_spec JSONB NOT NULL,

    desired_state TEXT NOT NULL CHECK (desired_state IN ('RUNNING', 'DELETED')),

    observed_status TEXT NOT NULL DEFAULT 'PENDING' CHECK (observed_status IN (
        'PENDING', 'PROVISIONING', 'STARTING', 'RUNNING',
        'DEGRADED', 'ERROR', 'DELETING', 'DELETED')),

    -- Capacity this instance holds against its organization's quota, copied
    -- from the plan at creation so a later plan change cannot alter accounting.
    -- Released (counted out) once deleted_at is set.
    reserved_cpu_millis INTEGER NOT NULL,
    reserved_memory_mib INTEGER NOT NULL,
    reserved_storage_gib INTEGER NOT NULL,

    generation BIGINT NOT NULL DEFAULT 1,
    applied_generation BIGINT NOT NULL DEFAULT 0,

    k8s_namespace TEXT NOT NULL,
    k8s_name TEXT NOT NULL,

    endpoint_host TEXT,
    endpoint_port INTEGER,

    last_error TEXT,
    error_count INTEGER NOT NULL DEFAULT 0,

    next_reconcile_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    lease_owner TEXT,
    lease_token BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,

    UNIQUE (k8s_namespace, k8s_name)
);

-- The reconciler's work queue: instances not yet fully deleted, by due time.
CREATE INDEX instances_due_idx ON instances (next_reconcile_at) WHERE deleted_at IS NULL;

-- A name is unique among an organization's live instances; it becomes reusable
-- once the old instance is fully deleted.
CREATE UNIQUE INDEX instances_org_active_name_idx
    ON instances (organization_id, name) WHERE deleted_at IS NULL;

CREATE TABLE operations (
    id UUID PRIMARY KEY,
    instance_id UUID NOT NULL REFERENCES instances(id),
    action TEXT NOT NULL CHECK (action IN ('CREATE', 'DELETE')),
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED')),
    target_generation BIGINT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX operations_instance_idx ON operations (instance_id);
CREATE INDEX operations_created_idx ON operations (created_at);

-- A replayed POST with the same key and the same normalized body returns the
-- original instance and operation; the same key with a different body is a
-- conflict.
CREATE TABLE idempotency_keys (
    organization_id UUID NOT NULL REFERENCES organizations(id),
    key             TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    instance_id     UUID NOT NULL REFERENCES instances(id),
    operation_id    UUID NOT NULL REFERENCES operations(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (organization_id, key)
);
