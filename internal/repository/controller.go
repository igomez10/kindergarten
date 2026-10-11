package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/internal/domain"
)

// NotifyChannel is where committed desired-state changes are announced. The
// controller LISTENs to wake up early; it never depends on it, because
// notifications are lost while it is disconnected. The periodic scan of
// next_reconcile_at is the durable path.
const NotifyChannel = "kindergarten_reconcile"

func notify(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, id.String())
	return err
}

// ClaimDue leases up to limit instances that are due for reconciliation and
// not leased by a live worker. Each claim increments lease_token, the fencing
// token every later write by this worker must present. SKIP LOCKED lets
// several workers claim concurrently without blocking or double-claiming.
func (r *Repo) ClaimDue(ctx context.Context, owner string, lease time.Duration, limit int) ([]domain.Instance, error) {
	rows, err := r.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM instances
			WHERE deleted_at IS NULL
			  AND next_reconcile_at <= NOW()
			  AND (lease_expires_at IS NULL OR lease_expires_at < NOW())
			ORDER BY next_reconcile_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE instances i
		SET lease_owner = $2,
		    lease_token = i.lease_token + 1,
		    lease_expires_at = NOW() + make_interval(secs => $3)
		FROM due WHERE i.id = due.id
		RETURNING `+prefixed("i.", instanceColumns), limit, owner, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Instance, error) { return scanInstance(row) })
}

func prefixed(prefix, cols string) string {
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		out += prefix + c
	}
	return out
}

func splitCols(cols string) []string {
	var out []string
	cur := ""
	for _, r := range cols {
		switch r {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ', '\n', '\t':
		default:
			cur += string(r)
		}
	}
	return append(out, cur)
}

// Outcome is the result of one reconciliation, written back atomically.
type Outcome struct {
	Status        string
	StatusMessage string // empty clears it
	LastError     string // empty clears it
	ErrorCount    int
	EndpointHost  *string
	EndpointPort  *int
	EndpointAddr  *string
	// Applied records that the instance's current generation was accepted by
	// Kubernetes.
	Applied bool
	// Deleted marks the instance fully deleted, releasing its quota and name.
	Deleted bool
	// OperationStatus, if set, completes the open operations for the current
	// desired state (domain.OpSucceeded or domain.OpFailed).
	OperationStatus string
	NextIn          time.Duration
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SaveOutcome writes a reconciliation result and releases the lease — but
// only if this worker still holds the lease it claimed (owner + fencing token)
// and the desired state has not changed since it was read (generation). It
// returns false when either check fails: the result is stale, nothing was
// written, and the next reconciliation starts from the newest desired state.
func (r *Repo) SaveOutcome(ctx context.Context, inst domain.Instance, o Outcome) (bool, error) {
	saved := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE instances SET
				observed_status = $5,
				status_message = $6,
				last_error = $7,
				error_count = $8,
				endpoint_host = $9,
				endpoint_port = $10,
				endpoint_address = $14,
				applied_generation = CASE WHEN $11 THEN generation ELSE applied_generation END,
				deleted_at = CASE WHEN $12 THEN NOW() ELSE deleted_at END,
				next_reconcile_at = NOW() + make_interval(secs => $13),
				lease_owner = NULL,
				lease_expires_at = NULL,
				updated_at = NOW()
			WHERE id = $1 AND generation = $2 AND lease_owner = $3 AND lease_token = $4`,
			inst.ID, inst.Generation, *inst.LeaseOwner, inst.LeaseToken,
			o.Status, nullable(o.StatusMessage), nullable(o.LastError), o.ErrorCount,
			o.EndpointHost, o.EndpointPort, o.Applied, o.Deleted, o.NextIn.Seconds(), o.EndpointAddr)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		saved = true

		action := domain.OpCreate
		if inst.DesiredState == domain.DesiredDeleted {
			action = domain.OpDelete
		}
		switch o.OperationStatus {
		case "":
			_, err = tx.Exec(ctx, `UPDATE operations SET status = 'RUNNING'
				WHERE instance_id = $1 AND action = $2 AND status = 'PENDING'`, inst.ID, action)
		default:
			_, err = tx.Exec(ctx, `UPDATE operations SET status = $3, error_message = $4, completed_at = NOW()
				WHERE instance_id = $1 AND action = $2 AND status IN ('PENDING', 'RUNNING')`,
				inst.ID, action, o.OperationStatus, nullable(o.LastError))
		}
		return err
	})
	return saved, err
}

// ReleaseLease gives up a lease without recording anything, and makes the
// instance due immediately. Used after a stale SaveOutcome.
func (r *Repo) ReleaseLease(ctx context.Context, inst domain.Instance) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE instances SET lease_owner = NULL, lease_expires_at = NULL, next_reconcile_at = NOW()
		WHERE id = $1 AND lease_owner = $2 AND lease_token = $3`, inst.ID, *inst.LeaseOwner, inst.LeaseToken)
	return err
}

// Pool exposes the connection pool for LISTEN.
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// ForceDue makes an instance due now. Tests and administrative nudges only.
func (r *Repo) ForceDue(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE instances SET next_reconcile_at = NOW() WHERE id = $1`, id)
	return err
}

// BumpGeneration records a desired-state change without altering the spec.
// Used by tests to simulate a concurrent update.
func (r *Repo) BumpGeneration(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE instances SET generation = generation + 1, next_reconcile_at = NOW() WHERE id = $1`, id)
	return err
}
