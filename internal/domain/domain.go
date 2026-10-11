// Package domain holds the platform's customer-facing vocabulary: plans,
// supported PostgreSQL versions, instance states and their validation. It
// knows nothing about HTTP, SQL or Kubernetes.
package domain

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Plan is a predefined instance size. Requests reference it by ID; the
// resources are never taken from the request.
type Plan struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CPUMillis   int    `json:"cpu_millis"`
	MemoryMiB   int    `json:"memory_mib"`
	StorageGiB  int    `json:"storage_gib"`
}

var plans = map[string]Plan{
	"nano":  {ID: "nano", DisplayName: "Nano", CPUMillis: 250, MemoryMiB: 512, StorageGiB: 5},
	"small": {ID: "small", DisplayName: "Small", CPUMillis: 500, MemoryMiB: 1024, StorageGiB: 10},
}

// Plans returns every plan, smallest first.
func Plans() []Plan {
	out := make([]Plan, 0, len(plans))
	for _, p := range plans {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPUMillis < out[j].CPUMillis })
	return out
}

func LookupPlan(id string) (Plan, bool) {
	p, ok := plans[id]
	return p, ok
}

// PostgresImages maps each supported major version to the pinned
// CloudNativePG operand image. Tags verified to exist; see
// docs/infrastructure.md.
var PostgresImages = map[string]string{
	"16": "ghcr.io/cloudnative-pg/postgresql:16.13-system-trixie",
	"17": "ghcr.io/cloudnative-pg/postgresql:17.10-system-trixie",
	"18": "ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie",
}

const DefaultPostgresVersion = "17"

// Desired state, what the user wants.
const (
	DesiredRunning = "RUNNING"
	DesiredDeleted = "DELETED"
)

// Observed status, what the reconciler last saw.
const (
	StatusPending      = "PENDING"
	StatusProvisioning = "PROVISIONING"
	StatusStarting     = "STARTING"
	StatusRunning      = "RUNNING"
	StatusDegraded     = "DEGRADED"
	StatusError        = "ERROR"
	StatusDeleting     = "DELETING"
	StatusDeleted      = "DELETED"
)

// Operation lifecycle.
const (
	OpCreate = "CREATE"
	OpDelete = "DELETE"

	OpPending   = "PENDING"
	OpRunning   = "RUNNING"
	OpSucceeded = "SUCCEEDED"
	OpFailed    = "FAILED"
)

// Spec is the stored desired_spec: only supported customer-facing settings,
// never Kubernetes fields.
type Spec struct {
	SchemaVersion   int    `json:"schema_version"`
	PostgresVersion string `json:"postgres_version"`
	Plan            string `json:"plan"`
	StorageGiB      int    `json:"storage_gib"`
}

// CreateRequest is the body of POST /v1/instances.
type CreateRequest struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Plan            string `json:"plan"`
	PostgresVersion string `json:"postgres_version"`
	StorageGiB      int    `json:"storage_gib"`
}

var nameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidationError is a client error: the request can never succeed as sent.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

// Normalize fills defaults and validates. The returned request is canonical:
// two requests meaning the same thing normalize identically, which is what
// idempotency hashing relies on.
func (r CreateRequest) Normalize() (CreateRequest, Plan, error) {
	if r.Type == "" {
		r.Type = "postgres"
	}
	if r.Type != "postgres" {
		return r, Plan{}, &ValidationError{"type", "only \"postgres\" is supported"}
	}
	if !nameRE.MatchString(r.Name) {
		return r, Plan{}, &ValidationError{"name", "must be 1-40 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen"}
	}
	plan, ok := LookupPlan(r.Plan)
	if !ok {
		return r, Plan{}, &ValidationError{"plan", fmt.Sprintf("unknown plan %q", r.Plan)}
	}
	if r.PostgresVersion == "" {
		r.PostgresVersion = DefaultPostgresVersion
	}
	if _, ok := PostgresImages[r.PostgresVersion]; !ok {
		return r, Plan{}, &ValidationError{"postgres_version", fmt.Sprintf("unsupported version %q", r.PostgresVersion)}
	}
	if r.StorageGiB == 0 {
		r.StorageGiB = plan.StorageGiB
	}
	if r.StorageGiB != plan.StorageGiB {
		return r, Plan{}, &ValidationError{"storage_gib", fmt.Sprintf("plan %q provides exactly %d GiB", plan.ID, plan.StorageGiB)}
	}
	return r, plan, nil
}

func (r CreateRequest) Spec() Spec {
	return Spec{SchemaVersion: 1, PostgresVersion: r.PostgresVersion, Plan: r.Plan, StorageGiB: r.StorageGiB}
}

// Default per-organization quota (plan §5.3).
const (
	DefaultMaxInstances  = 2
	DefaultMaxCPUMillis  = 1000
	DefaultMaxMemoryMiB  = 2048
	DefaultMaxStorageGiB = 20
)

// K8sName is the deterministic CloudNativePG Cluster name for an instance.
func K8sName(id uuid.UUID) string {
	return "pg-" + id.String()[:8]
}

// TenantNamespace derives an organization's namespace from its immutable ID.
func TenantNamespace(orgID uuid.UUID) string {
	return "tenant-" + orgID.String()[:8]
}

type Organization struct {
	ID            uuid.UUID
	Name          string
	K8sNamespace  string
	MaxInstances  int
	MaxCPUMillis  int
	MaxMemoryMiB  int
	MaxStorageGiB int
}

type User struct {
	ID      uuid.UUID
	Issuer  string
	Subject string
	Email   string
}

type Instance struct {
	ID                 uuid.UUID
	OrganizationID     uuid.UUID
	Name               string
	ServiceType        string
	Spec               Spec
	DesiredState       string
	ObservedStatus     string
	ReservedCPUMillis  int
	ReservedMemoryMiB  int
	ReservedStorageGiB int
	Generation         int64
	AppliedGeneration  int64
	K8sNamespace       string
	K8sName            string
	EndpointHost       *string
	EndpointPort       *int
	EndpointAddress    *string
	StatusMessage      *string
	LastError          *string
	ErrorCount         int
	NextReconcileAt    time.Time
	LeaseOwner         *string
	LeaseToken         int64
	LeaseExpiresAt     *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

type Operation struct {
	ID               uuid.UUID
	InstanceID       uuid.UUID
	Action           string
	Status           string
	TargetGeneration *int64
	ErrorMessage     *string
	CreatedAt        time.Time
	CompletedAt      *time.Time
}
