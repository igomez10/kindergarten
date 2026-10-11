// Package controller is the generic reconciliation loop: it turns rows in the
// metadata database into infrastructure through a Provisioner, and writes back
// what it observed. It holds no state of its own; a crash at any point is
// repaired by the next reconciliation of the same row.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
	"github.com/igomez10/kindergarten/internal/repository"
)

type Config struct {
	Owner            string        // this worker's identity in lease_owner
	Workers          int           // concurrent reconciliations
	LeaseDuration    time.Duration // must exceed ReconcileTimeout
	ReconcileTimeout time.Duration
	PollInterval     time.Duration // the durable fallback scan
}

func (c *Config) defaults() {
	if c.Workers == 0 {
		c.Workers = 4
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = 90 * time.Second
	}
	if c.ReconcileTimeout == 0 {
		c.ReconcileTimeout = 60 * time.Second
	}
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Second
	}
	// A reconciliation must finish (or be cancelled) well before its lease
	// can be taken over; the fencing token protects the writes regardless.
	if c.ReconcileTimeout > c.LeaseDuration*2/3 {
		c.ReconcileTimeout = c.LeaseDuration * 2 / 3
	}
}

// Tenants bootstraps an organization's namespace before anything is
// provisioned into it.
type Tenants interface {
	Ensure(ctx context.Context, org domain.Organization) error
}

type Controller struct {
	repo    *repository.Repo
	prov    provisioner.Provisioner
	tenants Tenants // nil: namespaces are administrator-made
	cfg     Config
	logger  *slog.Logger
	wake    chan struct{}
}

func New(repo *repository.Repo, prov provisioner.Provisioner, cfg Config, logger *slog.Logger) *Controller {
	cfg.defaults()
	return &Controller{repo: repo, prov: prov, cfg: cfg, logger: logger, wake: make(chan struct{}, 1)}
}

// WithTenants enables automatic tenant namespace bootstrap.
func (c *Controller) WithTenants(t Tenants) *Controller {
	c.tenants = t
	return c
}

// Run reconciles until ctx is cancelled, then waits for in-flight work.
func (c *Controller) Run(ctx context.Context) {
	slots := make(chan struct{}, c.cfg.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		free := c.cfg.Workers - len(slots)
		if free > 0 {
			insts, err := c.repo.ClaimDue(ctx, c.cfg.Owner, c.cfg.LeaseDuration, free)
			if err != nil && ctx.Err() == nil {
				c.logger.Error("claim due instances", "err", err)
			}
			for _, inst := range insts {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					c.Reconcile(ctx, inst)
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// Wake makes Run scan immediately instead of at the next poll.
func (c *Controller) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Listen wakes the loop on every NOTIFY from the API, reconnecting forever.
// Purely an accelerator: Run's poll still finds everything without it.
func (c *Controller) Listen(ctx context.Context, pool *pgxpool.Pool) {
	for ctx.Err() == nil {
		err := func() error {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "LISTEN "+repository.NotifyChannel); err != nil {
				return err
			}
			for {
				if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
					return err
				}
				c.Wake()
			}
		}()
		if ctx.Err() == nil {
			c.logger.Warn("listen for notifications", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// Schedule is how soon an instance in a status is looked at again (plan §3.6).
func Schedule(status string) time.Duration {
	switch status {
	case domain.StatusPending:
		return 0
	case domain.StatusProvisioning, domain.StatusStarting, domain.StatusDeleting:
		return 5 * time.Second
	case domain.StatusDegraded:
		return 10 * time.Second
	case domain.StatusError:
		return 5 * time.Minute
	default:
		return 60 * time.Second
	}
}

// Backoff is the retry delay after the n-th consecutive retryable failure:
// exponential from 2s with ±20 % jitter, capped at 60s so a fleet of
// failing instances does not retry in lockstep.
func Backoff(n int) time.Duration {
	d := 2 * time.Second * time.Duration(math.Pow(2, float64(min(n, 10)-1)))
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	return min(d, 60*time.Second)
}

const retryMessage = "A temporary infrastructure error occurred. The platform will retry automatically."

// Reconcile drives one leased instance one step toward its desired state and
// records the result. Exported for tests.
func (c *Controller) Reconcile(ctx context.Context, inst domain.Instance) {
	log := c.logger.With("instance_id", inst.ID, "organization_id", inst.OrganizationID,
		"generation", inst.Generation, "desired_state", inst.DesiredState, "lease_token", inst.LeaseToken)
	rctx, cancel := context.WithTimeout(ctx, c.cfg.ReconcileTimeout)
	defer cancel()

	start := time.Now()
	out, err := c.step(rctx, inst)
	if err != nil {
		out = c.failure(inst, err)
		if provisioner.IsPermanent(err) {
			log.Warn("reconcile failed permanently", "err", err)
		} else {
			log.Error("reconcile failed, will retry", "err", err, "retry_in", out.NextIn)
		}
	}

	// Write with a fresh context: a timed-out step must still record why.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer scancel()
	saved, err := c.repo.SaveOutcome(sctx, inst, out)
	switch {
	case err != nil:
		// The lease expires on its own and another pass retries.
		log.Error("save outcome", "err", err)
	case !saved:
		log.Info("outcome stale (desired state changed or lease lost); requeued")
		if err := c.repo.ReleaseLease(sctx, inst); err != nil {
			log.Error("release lease", "err", err)
		}
	default:
		log.Info("reconciled", "status", out.Status, "previous_status", inst.ObservedStatus,
			"next_in", out.NextIn.String(), "duration_ms", time.Since(start).Milliseconds())
	}
}

func (c *Controller) step(ctx context.Context, inst domain.Instance) (repository.Outcome, error) {
	if inst.DesiredState == domain.DesiredDeleted {
		done, err := c.prov.Delete(ctx, inst)
		if err != nil {
			return repository.Outcome{}, err
		}
		if done {
			return repository.Outcome{Status: domain.StatusDeleted, Deleted: true, OperationStatus: domain.OpSucceeded}, nil
		}
		return repository.Outcome{Status: domain.StatusDeleting, StatusMessage: "Deleting the database and its storage.", NextIn: Schedule(domain.StatusDeleting)}, nil
	}

	if c.tenants != nil {
		org, err := c.repo.GetOrganization(ctx, inst.OrganizationID)
		if err != nil {
			return repository.Outcome{}, err
		}
		// Instances placed in an administrator-made namespace
		// (NAMESPACE_OVERRIDE) are not the organization's to bootstrap.
		if org.K8sNamespace == inst.K8sNamespace {
			if err := c.tenants.Ensure(ctx, org); err != nil {
				return repository.Outcome{}, err
			}
		}
	}
	if err := c.prov.Apply(ctx, inst); err != nil {
		return repository.Outcome{}, err
	}
	obs, err := c.prov.Observe(ctx, inst)
	if err != nil {
		return repository.Outcome{}, err
	}
	out := repository.Outcome{
		Status:        obs.Status,
		StatusMessage: obs.Message,
		EndpointHost:  obs.EndpointHost,
		EndpointPort:  obs.EndpointPort,
		EndpointAddr:  obs.EndpointAddress,
		Applied:       true,
		NextIn:        Schedule(obs.Status),
	}
	if obs.Status == domain.StatusRunning {
		out.OperationStatus = domain.OpSucceeded
	}
	return out, nil
}

// failure turns an error into an outcome. Permanent errors surface as ERROR
// with an actionable message and are rechecked slowly; anything else keeps
// the last observed status and retries with backoff.
func (c *Controller) failure(inst domain.Instance, err error) repository.Outcome {
	out := repository.Outcome{
		Status:       inst.ObservedStatus,
		ErrorCount:   inst.ErrorCount + 1,
		EndpointHost: inst.EndpointHost,
		EndpointPort: inst.EndpointPort,
		EndpointAddr: inst.EndpointAddress,
	}
	if inst.StatusMessage != nil {
		out.StatusMessage = *inst.StatusMessage
	}
	var perm *provisioner.PermanentError
	if errors.As(err, &perm) {
		out.Status = domain.StatusError
		out.LastError = perm.Message
		out.OperationStatus = domain.OpFailed
		out.NextIn = Schedule(domain.StatusError)
		return out
	}
	if out.Status == domain.StatusPending {
		out.Status = domain.StatusProvisioning
	}
	if inst.DesiredState == domain.DesiredDeleted {
		out.Status = domain.StatusDeleting
	}
	out.LastError = retryMessage
	out.NextIn = Backoff(out.ErrorCount)
	return out
}
