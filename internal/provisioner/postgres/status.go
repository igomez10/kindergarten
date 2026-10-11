package postgres

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/igomez10/kindergarten/internal/domain"
)

// Facts is everything Observe gathers before deciding a status. Kept apart
// from the gathering so the decision is a pure, testable function.
type Facts struct {
	Cluster          *unstructured.Unstructured // nil if absent
	ExpectedImage    string
	SecretExists     bool
	RWEndpointReady  bool
	ConnectionErr    error // nil when the authenticated check passed; not run unless everything else is ready
	ConnectionTried  bool
	PendingReason    string // user-facing reason pods or volumes are stuck, if any
	PreviouslyActive bool   // the instance has been RUNNING or DEGRADED before

	// External exposure; only consulted when ExternalExpected.
	ExternalExpected bool
	ExternalIP       string
	DNSReady         bool
	ExternalTried    bool
	ExternalErr      error
}

// Decide maps the facts to a platform status and a user-facing message.
//
//	PROVISIONING  Cluster accepted, but no primary yet (storage, scheduling, initdb)
//	STARTING      primary exists, but not every readiness check passes yet
//	RUNNING       every check passes, including an authenticated connection
//	DEGRADED      was running, now some check fails
func Decide(f Facts) (status, message string) {
	notReady := func(msg string) (string, string) {
		if f.PreviouslyActive {
			return domain.StatusDegraded, msg
		}
		return domain.StatusStarting, msg
	}

	if f.Cluster == nil {
		if f.PreviouslyActive {
			return domain.StatusDegraded, "The database resource is missing; the platform is recreating it."
		}
		return domain.StatusProvisioning, "Creating the database."
	}

	instances, _, _ := unstructured.NestedInt64(f.Cluster.Object, "spec", "instances")
	ready, _, _ := unstructured.NestedInt64(f.Cluster.Object, "status", "readyInstances")
	primary, _, _ := unstructured.NestedString(f.Cluster.Object, "status", "currentPrimary")
	image, _, _ := unstructured.NestedString(f.Cluster.Object, "status", "image")

	if primary == "" || !conditionTrue(f.Cluster, "Initialized") && !f.PreviouslyActive {
		msg := "Provisioning storage and initializing the database."
		if f.PendingReason != "" {
			msg = f.PendingReason
		}
		if f.PreviouslyActive {
			return domain.StatusDegraded, msg
		}
		return domain.StatusProvisioning, msg
	}
	if ready < instances || !conditionTrue(f.Cluster, "Ready") {
		msg := "Waiting for the database to become ready."
		if f.PendingReason != "" {
			msg = f.PendingReason
		}
		return notReady(msg)
	}
	if image != f.ExpectedImage {
		return notReady("Applying the requested configuration.")
	}
	if !f.SecretExists {
		return notReady("Waiting for database credentials.")
	}
	if !f.RWEndpointReady {
		return notReady("Waiting for the database endpoint.")
	}
	if !f.ConnectionTried || f.ConnectionErr != nil {
		return notReady("Waiting for the database to accept connections.")
	}
	if f.ExternalExpected {
		if f.ExternalIP == "" {
			return notReady("Waiting for a network address.")
		}
		if !f.DNSReady {
			return notReady("Publishing the hostname.")
		}
		if !f.ExternalTried || f.ExternalErr != nil {
			return notReady("Waiting for the external endpoint to accept TLS connections.")
		}
	}
	return domain.StatusRunning, ""
}

func conditionTrue(u *unstructured.Unstructured, typ string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == typ {
			return m["status"] == "True"
		}
	}
	return false
}
