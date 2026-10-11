package postgres

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
)

// Ownership labels. Every object the platform creates carries them, and a
// Cluster without the matching instance ID is never modified.
const (
	LabelPrefix       = "pgcloud.igomez10.github.io/"
	LabelManaged      = LabelPrefix + "managed"
	LabelInstanceID   = LabelPrefix + "instance-id"
	LabelOrganization = LabelPrefix + "organization-id"

	FieldManager = "platform-controller"

	// The database and role every instance gets.
	AppDatabase = "app"
	AppUser     = "app"
)

var ClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

type Options struct {
	StorageClass string
	// DBDomain, when set, exposes each instance outside the cluster as
	// <k8s-name>.<DBDomain> on its own LoadBalancer address from AddressPool,
	// with that name in the server certificate. Empty keeps instances
	// cluster-internal.
	DBDomain    string
	AddressPool string
}

func (o Options) Exposed() bool { return o.DBDomain != "" }

// Hostname is the instance's external DNS name.
func Hostname(inst domain.Instance, opts Options) string {
	return inst.K8sName + "." + opts.DBDomain
}

func externalServiceName(inst domain.Instance) string { return inst.K8sName + "-external" }

func labels(inst domain.Instance) map[string]any {
	return map[string]any{
		LabelManaged:      "true",
		LabelInstanceID:   inst.ID.String(),
		LabelOrganization: inst.OrganizationID.String(),
	}
}

// BuildCluster translates an instance's desired spec into a CloudNativePG
// Cluster. Deterministic: the same instance always yields the same object.
func BuildCluster(inst domain.Instance, opts Options) (*unstructured.Unstructured, error) {
	image, ok := domain.PostgresImages[inst.Spec.PostgresVersion]
	if !ok {
		return nil, &provisioner.PermanentError{Message: fmt.Sprintf("PostgreSQL %s is no longer supported", inst.Spec.PostgresVersion)}
	}
	plan, ok := domain.LookupPlan(inst.Spec.Plan)
	if !ok {
		return nil, &provisioner.PermanentError{Message: fmt.Sprintf("plan %q no longer exists", inst.Spec.Plan)}
	}
	memory := fmt.Sprintf("%dMi", plan.MemoryMiB)
	obj := map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":      inst.K8sName,
			"namespace": inst.K8sNamespace,
			"labels":    labels(inst),
		},
		"spec": map[string]any{
			"instances": int64(1),
			"imageName": image,
			// Users only ever get the application role.
			"enableSuperuserAccess": false,
			"primaryUpdateStrategy": "unsupervised",
			"bootstrap": map[string]any{
				"initdb": map[string]any{"database": AppDatabase, "owner": AppUser},
			},
			"storage": map[string]any{
				"size":         fmt.Sprintf("%dGi", inst.Spec.StorageGiB),
				"storageClass": opts.StorageClass,
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": fmt.Sprintf("%dm", plan.CPUMillis), "memory": memory},
				"limits":   map[string]any{"memory": memory},
			},
			// Pods, PVCs and Services inherit the ownership labels, which is
			// what tenant NetworkPolicies and cleanup checks select on.
			"inheritedMetadata": map[string]any{"labels": labels(inst)},
			"monitoring":        map[string]any{"enablePodMonitor": false},
		},
	}
	if opts.Exposed() {
		spec := obj["spec"].(map[string]any)
		// CloudNativePG keeps issuing its own CA and server certificate; this
		// adds the external name so clients can use sslmode=verify-full.
		spec["certificates"] = map[string]any{"serverAltDNSNames": []any{Hostname(inst, opts)}}
		// An extra Service following the primary (selectorType rw), so it
		// survives failover without touching CNPG's own -rw Service.
		spec["managed"] = map[string]any{"services": map[string]any{"additional": []any{map[string]any{
			"selectorType": "rw",
			"serviceTemplate": map[string]any{
				"metadata": map[string]any{
					"name":        externalServiceName(inst),
					"labels":      labels(inst),
					"annotations": map[string]any{"metallb.io/address-pool": opts.AddressPool},
				},
				"spec": map[string]any{
					"type": "LoadBalancer",
					// Keeps the client's source address (tenant NetworkPolicy
					// admits outside clients by it) and has MetalLB announce
					// from the node actually running the primary.
					"externalTrafficPolicy": "Local",
				},
			},
		}}}}
	}
	return &unstructured.Unstructured{Object: obj}, nil
}
