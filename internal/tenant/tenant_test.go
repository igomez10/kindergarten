package tenant

import (
	"testing"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/igomez10/kindergarten/internal/domain"
)

func TestObjects(t *testing.T) {
	org := domain.Organization{ID: uuid.MustParse("a4b87944-1c4b-4aa2-b765-a2037d78fbd1"), K8sNamespace: "tenant-a4b87944",
		MaxInstances: 2, MaxCPUMillis: 1000, MaxMemoryMiB: 2048, MaxStorageGiB: 20}
	objs := New(nil, Options{PlatformNamespace: "platform-system", OperatorNamespace: "cnpg-system",
		SecretReaders: []string{"kindergarten-api", "kindergarten-controller"}, SecretReaderRole: "kindergarten-tenant-secrets-reader"}).Objects(org)
	byKind := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		byKind[o.GetKind()+"/"+o.GetName()] = o
		if o.GetKind() != "Namespace" && o.GetNamespace() != "tenant-a4b87944" {
			t.Errorf("%s/%s in namespace %q", o.GetKind(), o.GetName(), o.GetNamespace())
		}
	}
	ns := byKind["Namespace/tenant-a4b87944"]
	if ns.GetLabels()["pod-security.kubernetes.io/enforce"] != "restricted" || ns.GetLabels()["kindergarten.igomez10.github.io/organization-id"] != org.ID.String() {
		t.Errorf("namespace labels %v", ns.GetLabels())
	}
	hard, _, _ := unstructured.NestedStringMap(byKind["ResourceQuota/tenant-quota"].Object, "spec", "hard")
	for k, want := range map[string]string{"requests.cpu": "1000m", "requests.memory": "2048Mi", "requests.storage": "20Gi", "persistentvolumeclaims": "2", "services.loadbalancers": "2"} {
		if hard[k] != want {
			t.Errorf("quota %s = %q, want %q", k, hard[k], want)
		}
	}
	subjects, _, _ := unstructured.NestedSlice(byKind["RoleBinding/kindergarten-secret-readers"].Object, "subjects")
	if len(subjects) != 2 || subjects[0].(map[string]any)["namespace"] != "platform-system" {
		t.Errorf("rolebinding subjects %v", subjects)
	}
	for _, name := range []string{"NetworkPolicy/default-deny", "NetworkPolicy/allow-platform", "CiliumNetworkPolicy/allow-clients-and-apiserver", "LimitRange/tenant-limits"} {
		if byKind[name] == nil {
			t.Errorf("missing %s", name)
		}
	}
}
