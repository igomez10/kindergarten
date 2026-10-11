// Package repository is the only code that reads or writes the platform
// metadata database. Every multi-statement mutation runs in one transaction.
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/internal/domain"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrNameTaken           = errors.New("an instance with this name already exists")
)

// QuotaError says which organization limit a create would exceed.
type QuotaError struct {
	Resource string
	Limit    int
	Used     int
	Request  int
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("quota exceeded for %s: limit %d, in use %d, requested %d", e.Resource, e.Limit, e.Used, e.Request)
}

type Repo struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

func (r *Repo) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

// Membership is an organization together with the caller's role in it.
type Membership struct {
	Organization domain.Organization
	Role         string
}

// EnsureUser maps (issuer, subject) to a local user, creating the user and a
// personal organization on first sight, and returns the user's memberships,
// oldest first. Safe under concurrent first logins: the user insert is
// ON CONFLICT and the personal org is only created by whichever transaction
// actually inserted the user.
func (r *Repo) EnsureUser(ctx context.Context, issuer, subject, email string) (domain.User, []Membership, error) {
	var user domain.User
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		newID := uuid.New()
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO users (id, issuer, subject, email) VALUES ($1, $2, $3, NULLIF($4, ''))
			ON CONFLICT (issuer, subject) DO UPDATE
				SET email = COALESCE(NULLIF(EXCLUDED.email, ''), users.email)
			RETURNING id, issuer, subject, COALESCE(email, ''), (xmax = 0)`,
			newID, issuer, subject, email,
		).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email, &inserted)
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}
		if !inserted {
			return nil
		}
		orgID := uuid.New()
		name := email
		if name == "" {
			name = subject
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO organizations (id, name, k8s_namespace, max_instances, max_cpu_millis, max_memory_mib, max_storage_gib)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			orgID, name+" (personal)", domain.TenantNamespace(orgID),
			domain.DefaultMaxInstances, domain.DefaultMaxCPUMillis, domain.DefaultMaxMemoryMiB, domain.DefaultMaxStorageGiB,
		); err != nil {
			return fmt.Errorf("create personal organization: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, 'owner')`, orgID, user.ID)
		return err
	})
	if err != nil {
		return domain.User{}, nil, err
	}
	ms, err := r.memberships(ctx, user.ID)
	return user, ms, err
}

func (r *Repo) memberships(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT o.id, o.name, o.k8s_namespace, o.max_instances, o.max_cpu_millis, o.max_memory_mib, o.max_storage_gib, m.role
		FROM organization_members m JOIN organizations o ON o.id = m.organization_id
		WHERE m.user_id = $1
		ORDER BY m.created_at, o.id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Membership, error) {
		var m Membership
		o := &m.Organization
		err := row.Scan(&o.ID, &o.Name, &o.K8sNamespace, &o.MaxInstances, &o.MaxCPUMillis, &o.MaxMemoryMiB, &o.MaxStorageGiB, &m.Role)
		return m, err
	})
}

// Usage is an organization's reserved capacity: every instance not yet fully
// deleted, including those still provisioning or being deleted.
type Usage struct {
	Instances  int `json:"instances"`
	CPUMillis  int `json:"cpu_millis"`
	MemoryMiB  int `json:"memory_mib"`
	StorageGiB int `json:"storage_gib"`
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func usage(ctx context.Context, q querier, orgID uuid.UUID) (Usage, error) {
	var u Usage
	err := q.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(reserved_cpu_millis), 0), COALESCE(SUM(reserved_memory_mib), 0), COALESCE(SUM(reserved_storage_gib), 0)
		FROM instances WHERE organization_id = $1 AND deleted_at IS NULL`, orgID,
	).Scan(&u.Instances, &u.CPUMillis, &u.MemoryMiB, &u.StorageGiB)
	return u, err
}

func (r *Repo) Usage(ctx context.Context, orgID uuid.UUID) (Usage, error) {
	return usage(ctx, r.pool, orgID)
}

// CreateParams is a validated, normalized create request.
type CreateParams struct {
	OrganizationID uuid.UUID
	IdempotencyKey string // optional
	Request        domain.CreateRequest
	Plan           domain.Plan
	// Namespace the CloudNativePG Cluster goes into. Normally the
	// organization's own; overridable while tenancy is administrator-made
	// (phases 2–4 use platform-test).
	K8sNamespace string
}

// CreateResult is what POST /v1/instances returns. Replayed is true when an
// earlier request with the same idempotency key produced it.
type CreateResult struct {
	Instance  domain.Instance
	Operation domain.Operation
	Replayed  bool
}

func requestHash(req domain.CreateRequest) string {
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateInstance durably records the desired instance and its CREATE
// operation, or returns the original result for a replayed idempotency key.
// The organization row is locked first, which serializes every create in the
// organization: quota checks, name checks and idempotency lookups cannot race.
func (r *Repo) CreateInstance(ctx context.Context, p CreateParams) (CreateResult, error) {
	var res CreateResult
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var org domain.Organization
		err := tx.QueryRow(ctx, `
			SELECT id, max_instances, max_cpu_millis, max_memory_mib, max_storage_gib
			FROM organizations WHERE id = $1 FOR UPDATE`, p.OrganizationID,
		).Scan(&org.ID, &org.MaxInstances, &org.MaxCPUMillis, &org.MaxMemoryMiB, &org.MaxStorageGiB)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		hash := requestHash(p.Request)
		if p.IdempotencyKey != "" {
			var storedHash string
			var instID, opID uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT request_hash, instance_id, operation_id FROM idempotency_keys
				WHERE organization_id = $1 AND key = $2`, p.OrganizationID, p.IdempotencyKey,
			).Scan(&storedHash, &instID, &opID)
			switch {
			case err == nil:
				if storedHash != hash {
					return ErrIdempotencyConflict
				}
				if res.Instance, err = getInstance(ctx, tx, instID); err != nil {
					return err
				}
				if res.Operation, err = getOperation(ctx, tx, opID); err != nil {
					return err
				}
				res.Replayed = true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}

		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL)`,
			p.OrganizationID, p.Request.Name).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrNameTaken
		}

		u, err := usage(ctx, tx, p.OrganizationID)
		if err != nil {
			return err
		}
		for _, c := range []struct {
			name             string
			limit, used, req int
		}{
			{"instances", org.MaxInstances, u.Instances, 1},
			{"cpu_millis", org.MaxCPUMillis, u.CPUMillis, p.Plan.CPUMillis},
			{"memory_mib", org.MaxMemoryMiB, u.MemoryMiB, p.Plan.MemoryMiB},
			{"storage_gib", org.MaxStorageGiB, u.StorageGiB, p.Request.StorageGiB},
		} {
			if c.used+c.req > c.limit {
				return &QuotaError{Resource: c.name, Limit: c.limit, Used: c.used, Request: c.req}
			}
		}

		instID := uuid.New()
		spec, err := json.Marshal(p.Request.Spec())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO instances (id, organization_id, name, service_type, desired_spec, desired_state,
				reserved_cpu_millis, reserved_memory_mib, reserved_storage_gib, k8s_namespace, k8s_name)
			VALUES ($1, $2, $3, 'postgres', $4, $5, $6, $7, $8, $9, $10)`,
			instID, p.OrganizationID, p.Request.Name, spec, domain.DesiredRunning,
			p.Plan.CPUMillis, p.Plan.MemoryMiB, p.Request.StorageGiB, p.K8sNamespace, domain.K8sName(instID))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "instances_org_active_name_idx" {
				return ErrNameTaken
			}
			return fmt.Errorf("insert instance: %w", err)
		}

		opID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO operations (id, instance_id, action, target_generation) VALUES ($1, $2, $3, 1)`,
			opID, instID, domain.OpCreate); err != nil {
			return fmt.Errorf("insert operation: %w", err)
		}

		if err := notify(ctx, tx, instID); err != nil {
			return err
		}

		if p.IdempotencyKey != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO idempotency_keys (organization_id, key, request_hash, instance_id, operation_id)
				VALUES ($1, $2, $3, $4, $5)`, p.OrganizationID, p.IdempotencyKey, hash, instID, opID); err != nil {
				return fmt.Errorf("record idempotency key: %w", err)
			}
		}

		if res.Instance, err = getInstance(ctx, tx, instID); err != nil {
			return err
		}
		res.Operation, err = getOperation(ctx, tx, opID)
		return err
	})
	return res, err
}

const instanceColumns = `id, organization_id, name, service_type, desired_spec, desired_state, observed_status,
	reserved_cpu_millis, reserved_memory_mib, reserved_storage_gib, generation, applied_generation,
	k8s_namespace, k8s_name, endpoint_host, endpoint_port, endpoint_address, status_message, last_error, error_count, next_reconcile_at,
	lease_owner, lease_token, lease_expires_at, created_at, updated_at, deleted_at`

func scanInstance(row pgx.Row) (domain.Instance, error) {
	var i domain.Instance
	var spec []byte
	err := row.Scan(&i.ID, &i.OrganizationID, &i.Name, &i.ServiceType, &spec, &i.DesiredState, &i.ObservedStatus,
		&i.ReservedCPUMillis, &i.ReservedMemoryMiB, &i.ReservedStorageGiB, &i.Generation, &i.AppliedGeneration,
		&i.K8sNamespace, &i.K8sName, &i.EndpointHost, &i.EndpointPort, &i.EndpointAddress, &i.StatusMessage, &i.LastError, &i.ErrorCount, &i.NextReconcileAt,
		&i.LeaseOwner, &i.LeaseToken, &i.LeaseExpiresAt, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	if err := json.Unmarshal(spec, &i.Spec); err != nil {
		return i, fmt.Errorf("decode desired_spec of %s: %w", i.ID, err)
	}
	return i, nil
}

func getInstance(ctx context.Context, q querier, id uuid.UUID) (domain.Instance, error) {
	return scanInstance(q.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id = $1`, id))
}

const operationColumns = `id, instance_id, action, status, target_generation, error_message, created_at, completed_at`

func scanOperation(row pgx.Row) (domain.Operation, error) {
	var o domain.Operation
	err := row.Scan(&o.ID, &o.InstanceID, &o.Action, &o.Status, &o.TargetGeneration, &o.ErrorMessage, &o.CreatedAt, &o.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

func getOperation(ctx context.Context, q querier, id uuid.UUID) (domain.Operation, error) {
	return scanOperation(q.QueryRow(ctx, `SELECT `+operationColumns+` FROM operations WHERE id = $1`, id))
}

// GetInstance returns an instance only if it belongs to one of orgIDs, so a
// foreign ID is indistinguishable from a missing one.
func (r *Repo) GetInstance(ctx context.Context, id uuid.UUID, orgIDs []uuid.UUID) (domain.Instance, error) {
	return scanInstance(r.pool.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances
		WHERE id = $1 AND organization_id = ANY($2)`, id, orgIDs))
}

// ListInstances returns the organizations' instances that are not yet fully
// deleted, newest first.
func (r *Repo) ListInstances(ctx context.Context, orgIDs []uuid.UUID) ([]domain.Instance, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+instanceColumns+` FROM instances
		WHERE organization_id = ANY($1) AND deleted_at IS NULL ORDER BY created_at DESC, id`, orgIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Instance, error) { return scanInstance(row) })
}

// GetOperation returns an operation only if its instance belongs to one of orgIDs.
func (r *Repo) GetOperation(ctx context.Context, id uuid.UUID, orgIDs []uuid.UUID) (domain.Operation, error) {
	return scanOperation(r.pool.QueryRow(ctx, `SELECT o.id, o.instance_id, o.action, o.status, o.target_generation,
			o.error_message, o.created_at, o.completed_at
		FROM operations o JOIN instances i ON i.id = o.instance_id
		WHERE o.id = $1 AND i.organization_id = ANY($2)`, id, orgIDs))
}

// ListOperations returns an instance's operations, newest first.
func (r *Repo) ListOperations(ctx context.Context, instanceID uuid.UUID) ([]domain.Operation, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+operationColumns+` FROM operations
		WHERE instance_id = $1 ORDER BY created_at DESC, id LIMIT 50`, instanceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Operation, error) { return scanOperation(row) })
}

// RequestDelete records that the user wants the instance gone and returns its
// DELETE operation. Deleting an instance that is already being deleted returns
// the existing operation. The generation bump makes any in-flight
// reconciliation of the old desired state stale, so its result is discarded
// (SaveOutcome) and deletion wins even mid-provisioning.
func (r *Repo) RequestDelete(ctx context.Context, id uuid.UUID, orgIDs []uuid.UUID) (domain.Instance, domain.Operation, error) {
	var inst domain.Instance
	var op domain.Operation
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		inst, err = scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances
			WHERE id = $1 AND organization_id = ANY($2) AND deleted_at IS NULL FOR UPDATE`, id, orgIDs))
		if err != nil {
			return err
		}
		if inst.DesiredState == domain.DesiredDeleted {
			op, err = scanOperation(tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM operations
				WHERE instance_id = $1 AND action = 'DELETE' ORDER BY created_at DESC LIMIT 1`, id))
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE instances SET desired_state = 'DELETED', observed_status = 'DELETING',
				status_message = 'Deletion requested.', last_error = NULL, error_count = 0,
				generation = generation + 1, next_reconcile_at = NOW(), updated_at = NOW()
			WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE operations SET status = 'FAILED', error_message = 'Superseded by a delete request.', completed_at = NOW()
			WHERE instance_id = $1 AND action = 'CREATE' AND status IN ('PENDING', 'RUNNING')`, id); err != nil {
			return err
		}
		opID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO operations (id, instance_id, action, target_generation)
			SELECT $1, id, 'DELETE', generation FROM instances WHERE id = $2`, opID, id); err != nil {
			return err
		}
		if err := notify(ctx, tx, id); err != nil {
			return err
		}
		if inst, err = getInstance(ctx, tx, id); err != nil {
			return err
		}
		op, err = getOperation(ctx, tx, opID)
		return err
	})
	return inst, op, err
}

func (r *Repo) GetOrganization(ctx context.Context, id uuid.UUID) (domain.Organization, error) {
	var o domain.Organization
	err := r.pool.QueryRow(ctx, `SELECT id, name, k8s_namespace, max_instances, max_cpu_millis, max_memory_mib, max_storage_gib
		FROM organizations WHERE id = $1`, id).Scan(&o.ID, &o.Name, &o.K8sNamespace, &o.MaxInstances, &o.MaxCPUMillis, &o.MaxMemoryMiB, &o.MaxStorageGiB)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}
