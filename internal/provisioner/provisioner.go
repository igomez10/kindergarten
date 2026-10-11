// Package provisioner defines what the generic controller needs from a
// managed-service implementation. The controller knows nothing about
// PostgreSQL; internal/provisioner/postgres knows nothing about leases,
// retries or the metadata database.
package provisioner

import (
	"context"
	"errors"

	"github.com/igomez10/kindergarten/internal/domain"
)

// ObservedState is what the provisioner saw in Kubernetes.
type ObservedState struct {
	Status string // one of domain.Status*
	// Message is a sanitized, user-facing explanation of a non-RUNNING state
	// ("Waiting for storage to be provisioned."). Never Kubernetes internals
	// or secret values.
	Message      string
	EndpointHost *string
	EndpointPort *int
	// EndpointAddress is the IP behind EndpointHost, when externally exposed.
	EndpointAddress *string
}

type Provisioner interface {
	// Apply makes the instance's Kubernetes resources match its desired spec.
	// Idempotent: safe to call on every reconciliation.
	Apply(ctx context.Context, inst domain.Instance) error
	// Observe reads the actual state. It does not mutate anything.
	Observe(ctx context.Context, inst domain.Instance) (ObservedState, error)
	// Delete removes the instance's resources and reports whether everything,
	// storage included, is gone. Called repeatedly until it returns true.
	Delete(ctx context.Context, inst domain.Instance) (done bool, err error)
}

// PermanentError is a failure retrying cannot fix without a change by a user
// or administrator: an invalid spec, or a resource the platform does not own
// sitting where its own should be. Message is safe to show to the user.
type PermanentError struct {
	Message string
	Err     error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *PermanentError) Unwrap() error { return e.Err }

func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}
