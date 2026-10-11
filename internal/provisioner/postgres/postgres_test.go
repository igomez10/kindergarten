package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
)

func inst(plan, version string, storage int) domain.Instance {
	id := uuid.MustParse("7d8f2b6c-3c54-4f83-9e1f-4e8599b7bc24")
	return domain.Instance{
		ID: id, OrganizationID: uuid.MustParse("a4b87944-1c4b-4aa2-b765-a2037d78fbd1"),
		Spec:         domain.Spec{SchemaVersion: 1, Plan: plan, PostgresVersion: version, StorageGiB: storage},
		K8sNamespace: "tenant-a4b87944", K8sName: domain.K8sName(id),
	}
}

func TestBuildClusterMatchesPlan(t *testing.T) {
	for _, tc := range []struct {
		plan, cpu, mem, storage string
		gib                     int
	}{
		{"nano", "250m", "512Mi", "5Gi", 5},
		{"small", "500m", "1024Mi", "10Gi", 10},
	} {
		u, err := BuildCluster(inst(tc.plan, "17", tc.gib), Options{StorageClass: "longhorn"})
		if err != nil {
			t.Fatal(err)
		}
		get := func(path ...string) any {
			v, _, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
			return v
		}
		if u.GetName() != "pg-7d8f2b6c" || u.GetNamespace() != "tenant-a4b87944" {
			t.Errorf("identity %s/%s", u.GetNamespace(), u.GetName())
		}
		if u.GetLabels()[LabelInstanceID] != "7d8f2b6c-3c54-4f83-9e1f-4e8599b7bc24" || u.GetLabels()[LabelManaged] != "true" {
			t.Errorf("labels %v", u.GetLabels())
		}
		checks := map[string][2]any{
			"cpu":     {get("spec", "resources", "requests", "cpu"), tc.cpu},
			"mem":     {get("spec", "resources", "requests", "memory"), tc.mem},
			"limit":   {get("spec", "resources", "limits", "memory"), tc.mem},
			"storage": {get("spec", "storage", "size"), tc.storage},
			"sc":      {get("spec", "storage", "storageClass"), "longhorn"},
			"image":   {get("spec", "imageName"), "ghcr.io/cloudnative-pg/postgresql:17.10-system-trixie"},
			"su":      {get("spec", "enableSuperuserAccess"), false},
			"db":      {get("spec", "bootstrap", "initdb", "database"), "app"},
		}
		for name, c := range checks {
			if c[0] != c[1] {
				t.Errorf("%s %s: got %v want %v", tc.plan, name, c[0], c[1])
			}
		}
	}
}

func TestBuildClusterIsDeterministic(t *testing.T) {
	a, _ := BuildCluster(inst("small", "17", 10), Options{StorageClass: "longhorn"})
	b, _ := BuildCluster(inst("small", "17", 10), Options{StorageClass: "longhorn"})
	ja, _ := a.MarshalJSON()
	jb, _ := b.MarshalJSON()
	if string(ja) != string(jb) {
		t.Fatal("same instance produced different manifests")
	}
}

func TestBuildClusterUnsupportedVersionIsPermanent(t *testing.T) {
	_, err := BuildCluster(inst("small", "9.6", 10), Options{})
	if !provisioner.IsPermanent(err) {
		t.Fatalf("got %v", err)
	}
}

func cluster(ready int64, primary, image string, conds map[string]string) *unstructured.Unstructured {
	var cs []any
	for typ, st := range conds {
		cs = append(cs, map[string]any{"type": typ, "status": st})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"instances": int64(1)},
		"status": map[string]any{"readyInstances": ready, "currentPrimary": primary, "image": image, "conditions": cs},
	}}
}

const img = "ghcr.io/cloudnative-pg/postgresql:17.10-system-trixie"

func TestDecide(t *testing.T) {
	healthy := cluster(1, "pg-1", img, map[string]string{"Initialized": "True", "Ready": "True"})
	all := Facts{Cluster: healthy, ExpectedImage: img, SecretExists: true, RWEndpointReady: true, ConnectionTried: true}
	mod := func(f func(*Facts)) Facts { c := all; f(&c); return c }

	for name, tc := range map[string]struct {
		f    Facts
		want string
	}{
		"missing, new":         {Facts{ExpectedImage: img}, domain.StatusProvisioning},
		"missing, was running": {Facts{ExpectedImage: img, PreviouslyActive: true}, domain.StatusDegraded},
		"no primary yet":       {mod(func(f *Facts) { f.Cluster = cluster(0, "", "", nil) }), domain.StatusProvisioning},
		"primary, not ready":   {mod(func(f *Facts) { f.Cluster = cluster(0, "pg-1", img, map[string]string{"Initialized": "True"}) }), domain.StatusStarting},
		"not ready, was running": {mod(func(f *Facts) {
			f.Cluster = cluster(0, "pg-1", img, map[string]string{"Initialized": "True"})
			f.PreviouslyActive = true
		}), domain.StatusDegraded},
		"old image":                {mod(func(f *Facts) { f.ExpectedImage = "other" }), domain.StatusStarting},
		"no secret":                {mod(func(f *Facts) { f.SecretExists = false }), domain.StatusStarting},
		"no endpoint":              {mod(func(f *Facts) { f.RWEndpointReady = false }), domain.StatusStarting},
		"connection fails":         {mod(func(f *Facts) { f.ConnectionErr = errors.New("refused") }), domain.StatusStarting},
		"connection not tried":     {mod(func(f *Facts) { f.ConnectionTried = false }), domain.StatusStarting},
		"connection fails, was up": {mod(func(f *Facts) { f.ConnectionErr = errors.New("x"); f.PreviouslyActive = true }), domain.StatusDegraded},
		"all good":                 {all, domain.StatusRunning},
	} {
		if got, _ := Decide(tc.f); got != tc.want {
			t.Errorf("%s: got %s want %s", name, got, tc.want)
		}
	}
}

func TestPVCBelongsTo(t *testing.T) {
	for pvc, want := range map[string]bool{
		"pg-7d8f2b6c-1": true, "pg-7d8f2b6c-12": true, "pg-7d8f2b6c-1-wal": true,
		"pg-7d8f2b6c": false, "pg-7d8f2b6c-x": false, "pg-7d8f2b6cd-1": false, "pg-7d8f2b6c--wal": false,
	} {
		if got := pvcBelongsTo(pvc, "pg-7d8f2b6c"); got != want {
			t.Errorf("%s: %v", pvc, got)
		}
	}
}

func TestBuildClusterExposed(t *testing.T) {
	u, err := BuildCluster(inst("nano", "17", 5), Options{StorageClass: "longhorn", DBDomain: "db.example.internal", AddressPool: "kindergarten-db"})
	if err != nil {
		t.Fatal(err)
	}
	sans, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "certificates", "serverAltDNSNames")
	if len(sans) != 1 || sans[0] != "pg-7d8f2b6c.db.example.internal" {
		t.Errorf("SANs %v", sans)
	}
	svcs, _, _ := unstructured.NestedSlice(u.Object, "spec", "managed", "services", "additional")
	svc := svcs[0].(map[string]any)
	tmpl := svc["serviceTemplate"].(map[string]any)
	if svc["selectorType"] != "rw" ||
		tmpl["metadata"].(map[string]any)["name"] != "pg-7d8f2b6c-external" ||
		tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)["metallb.io/address-pool"] != "kindergarten-db" ||
		tmpl["spec"].(map[string]any)["type"] != "LoadBalancer" ||
		tmpl["spec"].(map[string]any)["externalTrafficPolicy"] != "Local" {
		t.Errorf("external service %v", svc)
	}
	internal, _ := BuildCluster(inst("nano", "17", 5), Options{StorageClass: "longhorn"})
	if _, found, _ := unstructured.NestedFieldNoCopy(internal.Object, "spec", "managed"); found {
		t.Error("unexposed cluster has an external service")
	}
}

func TestDecideExternal(t *testing.T) {
	healthy := cluster(1, "pg-1", img, map[string]string{"Initialized": "True", "Ready": "True"})
	base := Facts{Cluster: healthy, ExpectedImage: img, SecretExists: true, RWEndpointReady: true, ConnectionTried: true,
		ExternalExpected: true, ExternalIP: "10.0.0.220", DNSReady: true, ExternalTried: true}
	mod := func(f func(*Facts)) Facts { c := base; f(&c); return c }
	for name, tc := range map[string]struct {
		f    Facts
		want string
		msg  string
	}{
		"no ip":        {mod(func(f *Facts) { f.ExternalIP = "" }), domain.StatusStarting, "Waiting for a network address."},
		"no dns":       {mod(func(f *Facts) { f.DNSReady = false }), domain.StatusStarting, "Publishing the hostname."},
		"tls fails":    {mod(func(f *Facts) { f.ExternalErr = errors.New("x509") }), domain.StatusStarting, "Waiting for the external endpoint to accept TLS connections."},
		"tls fails up": {mod(func(f *Facts) { f.ExternalErr = errors.New("x"); f.PreviouslyActive = true }), domain.StatusDegraded, ""},
		"all good":     {base, domain.StatusRunning, ""},
	} {
		got, msg := Decide(tc.f)
		if got != tc.want || (tc.msg != "" && msg != tc.msg) {
			t.Errorf("%s: got %s %q", name, got, msg)
		}
	}
}
