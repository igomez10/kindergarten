// Package tenant bootstraps an organization's namespace: the namespace itself,
// a ResourceQuota mirroring the organization's platform quota, a LimitRange,
// and network isolation. Every object is server-side applied, so Ensure is
// idempotent and repairs drift on each call.
package tenant

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
	"github.com/igomez10/kindergarten/internal/provisioner/postgres"
)

var (
	namespaceGVR    = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	quotaGVR        = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}
	limitRangeGVR   = schema.GroupVersionResource{Version: "v1", Resource: "limitranges"}
	netpolGVR       = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
	ciliumNetpolGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	roleBindingGVR  = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
)

type Options struct {
	// Namespace the platform's own components run in; its pods may reach
	// tenant databases (the controller's connection check).
	PlatformNamespace string
	// Namespace of the CloudNativePG operator, which must reach every instance.
	OperatorNamespace string
	// SecretReaders are the platform ServiceAccounts (in PlatformNamespace)
	// granted SecretReaderRole in each tenant namespace — and only there, so
	// neither the API nor the controller can read Secrets cluster-wide.
	SecretReaders    []string
	SecretReaderRole string
}

type Bootstrapper struct {
	dyn  dynamic.Interface
	opts Options
}

func New(dyn dynamic.Interface, opts Options) *Bootstrapper {
	return &Bootstrapper{dyn: dyn, opts: opts}
}

func obj(apiVersion, kind, name, namespace string, rest map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "labels": map[string]any{postgres.LabelManaged: "true"}}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	for k, v := range rest {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

// Objects returns everything a tenant namespace consists of. Exported for tests.
func (b *Bootstrapper) Objects(org domain.Organization) []*unstructured.Unstructured {
	ns := org.K8sNamespace
	nsObj := obj("v1", "Namespace", ns, "", nil)
	nsObj.SetLabels(map[string]string{
		postgres.LabelManaged:      "true",
		postgres.LabelOrganization: org.ID.String(),
		// Database pods only; CloudNativePG's pods satisfy restricted.
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
	})

	quota := obj("v1", "ResourceQuota", "tenant-quota", ns, map[string]any{"spec": map[string]any{"hard": map[string]any{
		// Mirrors the platform quota the API enforces transactionally, as a
		// second, independent layer.
		"requests.cpu":           fmt.Sprintf("%dm", org.MaxCPUMillis),
		"requests.memory":        fmt.Sprintf("%dMi", org.MaxMemoryMiB),
		"limits.memory":          fmt.Sprintf("%dMi", org.MaxMemoryMiB),
		"requests.storage":       fmt.Sprintf("%dGi", org.MaxStorageGiB),
		"persistentvolumeclaims": fmt.Sprint(org.MaxInstances),
		"services.loadbalancers": fmt.Sprint(org.MaxInstances),
		"pods":                   fmt.Sprint(org.MaxInstances * 3), // instance + initdb/join job, plus headroom
	}}})

	limits := obj("v1", "LimitRange", "tenant-limits", ns, map[string]any{"spec": map[string]any{"limits": []any{
		map[string]any{
			"type":           "Container",
			"defaultRequest": map[string]any{"cpu": "50m", "memory": "64Mi"},
			"default":        map[string]any{"memory": "256Mi"},
		},
	}}})

	// Default deny, then the exact flows databases need.
	deny := obj("networking.k8s.io/v1", "NetworkPolicy", "default-deny", ns, map[string]any{"spec": map[string]any{
		"podSelector": map[string]any{},
		"policyTypes": []any{"Ingress", "Egress"},
	}})
	nsSel := func(name string) map[string]any {
		return map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": name}}}
	}
	allow := obj("networking.k8s.io/v1", "NetworkPolicy", "allow-platform", ns, map[string]any{"spec": map[string]any{
		"podSelector": map[string]any{},
		"policyTypes": []any{"Ingress", "Egress"},
		"ingress": []any{
			// The operator talks to every instance manager (status on 8000).
			map[string]any{"from": []any{nsSel(b.opts.OperatorNamespace)}},
			// The controller's authenticated connection check.
			map[string]any{"from": []any{nsSel(b.opts.PlatformNamespace)}, "ports": []any{map[string]any{"protocol": "TCP", "port": int64(5432)}}},
			// Pods of the same organization (replicas, jobs).
			map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{}}}},
		},
		"egress": []any{
			map[string]any{
				"to": []any{map[string]any{
					"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
					"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}},
				}},
				"ports": []any{map[string]any{"protocol": "UDP", "port": int64(53)}, map[string]any{"protocol": "TCP", "port": int64(53)}},
			},
			map[string]any{"to": []any{map[string]any{"podSelector": map[string]any{}}}},
		},
	}})
	// What plain NetworkPolicy cannot say under Cilium: the API server
	// (instance managers watch their Cluster), and clients from outside the
	// cluster. LAN clients arrive with their own address ("world", kept by
	// externalTrafficPolicy: Local); tailnet clients are masqueraded by the
	// subnet router k8s-04, a node ("host"/"remote-node"). Only port 5432.
	cilium := obj("cilium.io/v2", "CiliumNetworkPolicy", "allow-clients-and-apiserver", ns, map[string]any{"spec": map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{"cnpg.io/podRole": "instance"}},
		"ingress": []any{map[string]any{
			"fromEntities": []any{"world", "host", "remote-node"},
			"toPorts":      []any{map[string]any{"ports": []any{map[string]any{"port": "5432", "protocol": "TCP"}}}},
		}},
		"egress": []any{map[string]any{"toEntities": []any{"kube-apiserver"}}},
	}})
	// Jobs (initdb) also need the API server.
	ciliumJobs := obj("cilium.io/v2", "CiliumNetworkPolicy", "allow-apiserver", ns, map[string]any{"spec": map[string]any{
		"endpointSelector": map[string]any{},
		"egress":           []any{map[string]any{"toEntities": []any{"kube-apiserver"}}},
	}})
	objs := []*unstructured.Unstructured{nsObj, quota, limits, deny, allow, cilium, ciliumJobs}
	if len(b.opts.SecretReaders) > 0 {
		subjects := []any{}
		for _, sa := range b.opts.SecretReaders {
			subjects = append(subjects, map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": b.opts.PlatformNamespace})
		}
		objs = append(objs, obj("rbac.authorization.k8s.io/v1", "RoleBinding", "pgcloud-secret-readers", ns, map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": b.opts.SecretReaderRole},
			"subjects": subjects,
		}))
	}
	return objs
}

func gvrFor(u *unstructured.Unstructured) schema.GroupVersionResource {
	switch u.GetKind() {
	case "Namespace":
		return namespaceGVR
	case "ResourceQuota":
		return quotaGVR
	case "LimitRange":
		return limitRangeGVR
	case "NetworkPolicy":
		return netpolGVR
	case "RoleBinding":
		return roleBindingGVR
	default:
		return ciliumNetpolGVR
	}
}

// Ensure applies the tenant's namespace and policies. It refuses to adopt a
// namespace that exists without the platform's ownership label.
func (b *Bootstrapper) Ensure(ctx context.Context, org domain.Organization) error {
	existing, err := b.dyn.Resource(namespaceGVR).Get(ctx, org.K8sNamespace, metav1.GetOptions{})
	switch {
	case err == nil:
		if existing.GetLabels()[postgres.LabelOrganization] != org.ID.String() {
			return &provisioner.PermanentError{Message: "The organization's namespace is held by something the platform does not manage. An administrator must resolve the conflict."}
		}
	case apierrors.IsNotFound(err):
	default:
		return fmt.Errorf("get namespace: %w", err)
	}
	for _, o := range b.Objects(org) {
		r := b.dyn.Resource(gvrFor(o))
		var ri dynamic.ResourceInterface = r
		if o.GetNamespace() != "" {
			ri = r.Namespace(o.GetNamespace())
		}
		if _, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: postgres.FieldManager, Force: true}); err != nil {
			return fmt.Errorf("apply %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}
