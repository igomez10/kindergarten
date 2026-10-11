package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/db"
	"github.com/igomez10/kindergarten/internal/api"
	"github.com/igomez10/kindergarten/internal/auth"
	"github.com/igomez10/kindergarten/internal/repository"
	"github.com/igomez10/kindergarten/internal/testdb"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func newEnv(t *testing.T) *env {
	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.New(repository.New(pool), auth.Dev(), api.Config{NamespaceOverride: "platform-test"}, logger).Routes()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, pool: pool, srv: srv}
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *env) do(method, path, user string, body any, headers map[string]string) resp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Dev-User", user)
		req.Header.Set("X-Dev-Email", user+"@example.com")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return out
}

func (e *env) count(table string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func small(name string) map[string]any {
	return map[string]any{"name": name, "type": "postgres", "plan": "small", "postgres_version": "17", "storage_gib": 10}
}

func nano(name string) map[string]any {
	return map[string]any{"name": name, "plan": "nano"}
}

func field(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		cur = cur.(map[string]any)[p]
	}
	return cur
}

func TestCreateValidInstance(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/instances", "alice", small("development-db"), map[string]string{"Idempotency-Key": "k1"})
	if r.status != http.StatusAccepted {
		t.Fatalf("status %d: %v", r.status, r.body)
	}
	if got := field(r.body, "instance", "status"); got != "PENDING" {
		t.Errorf("instance status %v", got)
	}
	if got := field(r.body, "operation", "status"); got != "PENDING" {
		t.Errorf("operation status %v", got)
	}
	if e.count("instances") != 1 || e.count("operations") != 1 {
		t.Fatalf("want exactly one instance and one operation, got %d/%d", e.count("instances"), e.count("operations"))
	}

	id := field(r.body, "instance", "id").(string)
	g := e.do("GET", "/v1/instances/"+id, "alice", nil, nil)
	if g.status != 200 || g.body["status"] != "PENDING" || g.body["desired_state"] != "RUNNING" || g.body["endpoint"] != nil {
		t.Fatalf("get: %d %v", g.status, g.body)
	}
	if g.body["plan"] != "small" || g.body["postgres_version"] != "17" || g.body["cpu_millis"].(float64) != 500 {
		t.Errorf("get spec: %v", g.body)
	}

	var ns, k8sName string
	_ = e.pool.QueryRow(context.Background(), "SELECT k8s_namespace, k8s_name FROM instances").Scan(&ns, &k8sName)
	if ns != "platform-test" || k8sName != "pg-"+id[:8] {
		t.Errorf("k8s identity %s/%s", ns, k8sName)
	}

	op := e.do("GET", "/v1/operations/"+field(r.body, "operation", "id").(string), "alice", nil, nil)
	if op.status != 200 || op.body["action"] != "CREATE" {
		t.Errorf("operation: %d %v", op.status, op.body)
	}
	l := e.do("GET", "/v1/instances", "alice", nil, nil)
	if n := len(l.body["instances"].([]any)); n != 1 {
		t.Errorf("list has %d instances", n)
	}
}

func TestIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	h := map[string]string{"Idempotency-Key": "request-abc123"}
	first := e.do("POST", "/v1/instances", "alice", small("db"), h)
	// Same meaning, different spelling: defaults left implicit.
	second := e.do("POST", "/v1/instances", "alice", map[string]any{"name": "db", "plan": "small"}, h)
	if first.status != 202 || second.status != 202 {
		t.Fatalf("statuses %d %d: %v", first.status, second.status, second.body)
	}
	if field(first.body, "instance", "id") != field(second.body, "instance", "id") ||
		field(first.body, "operation", "id") != field(second.body, "operation", "id") {
		t.Fatalf("replay returned a different result: %v vs %v", first.body, second.body)
	}
	if second.header.Get("Idempotent-Replayed") != "true" {
		t.Error("replay not flagged")
	}
	if e.count("instances") != 1 || e.count("operations") != 1 {
		t.Fatal("replay created rows")
	}

	conflict := e.do("POST", "/v1/instances", "alice", small("other-db"), h)
	if conflict.status != http.StatusConflict {
		t.Fatalf("same key, different body: %d %v", conflict.status, conflict.body)
	}

	// Keys are per organization: Bob may use the same key.
	bob := e.do("POST", "/v1/instances", "bob", small("db"), h)
	if bob.status != 202 || field(bob.body, "instance", "id") == field(first.body, "instance", "id") {
		t.Fatalf("bob: %d %v", bob.status, bob.body)
	}
}

func TestConcurrentSameIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "/v1/me", "alice", nil, nil) // create the user up front
	var wg sync.WaitGroup
	ids := make([]any, 10)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.do("POST", "/v1/instances", "alice", nano("db"), map[string]string{"Idempotency-Key": "same"})
			if r.status == 202 {
				ids[i] = field(r.body, "instance", "id")
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || id == nil {
			t.Fatalf("divergent results: %v", ids)
		}
	}
	if e.count("instances") != 1 {
		t.Fatalf("%d instances", e.count("instances"))
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	cases := map[string]map[string]any{
		"unknown plan":        {"name": "db", "plan": "huge"},
		"unsupported version": {"name": "db", "plan": "small", "postgres_version": "9.6"},
		"wrong storage":       {"name": "db", "plan": "small", "storage_gib": 500},
		"bad name":            {"name": "Bad_Name", "plan": "small"},
		"empty name":          {"plan": "small"},
		"other type":          {"name": "db", "type": "redis", "plan": "small"},
		"unknown field":       {"name": "db", "plan": "small", "replicas": 3},
	}
	for name, body := range cases {
		r := e.do("POST", "/v1/instances", "alice", body, nil)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: status %d %v", name, r.status, r.body)
		}
	}
	if e.count("instances") != 0 {
		t.Fatal("invalid requests created rows")
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/plans", "/v1/instances"} {
		if r := e.do("GET", path, "", nil, nil); r.status != http.StatusUnauthorized {
			t.Errorf("%s without identity: %d", path, r.status)
		}
	}
	if r := e.do("GET", "/healthz", "", nil, nil); r.status != http.StatusNoContent {
		t.Errorf("healthz: %d", r.status)
	}
	if r := e.do("GET", "/readyz", "", nil, nil); r.status != http.StatusNoContent {
		t.Errorf("readyz: %d", r.status)
	}
}

func TestPersonalOrganization(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/v1/me", "alice", nil, nil)
	orgs := r.body["organizations"].([]any)
	if r.status != 200 || len(orgs) != 1 {
		t.Fatalf("me: %d %v", r.status, r.body)
	}
	org := orgs[0].(map[string]any)
	if org["role"] != "owner" || field(org, "quota", "instances").(float64) != 2 {
		t.Errorf("org: %v", org)
	}
	e.do("GET", "/v1/me", "alice", nil, nil)
	if e.count("organizations") != 1 || e.count("users") != 1 {
		t.Fatal("second login created another user or organization")
	}
}

func TestCrossTenantAccess(t *testing.T) {
	e := newEnv(t)
	a := e.do("POST", "/v1/instances", "alice", small("alice-db"), nil)
	id := field(a.body, "instance", "id").(string)
	opID := field(a.body, "operation", "id").(string)

	if r := e.do("GET", "/v1/instances/"+id, "bob", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("bob reads alice's instance: %d", r.status)
	}
	if r := e.do("GET", "/v1/operations/"+opID, "bob", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("bob reads alice's operation: %d", r.status)
	}
	if r := e.do("GET", "/v1/instances/"+id+"/operations", "bob", nil, nil); r.status != http.StatusNotFound {
		t.Errorf("bob lists alice's operations: %d", r.status)
	}
	if r := e.do("GET", "/v1/instances", "bob", nil, nil); len(r.body["instances"].([]any)) != 0 {
		t.Errorf("bob's list shows %v", r.body)
	}
	aliceOrg := field(e.do("GET", "/v1/me", "alice", nil, nil).body["organizations"].([]any)[0].(map[string]any), "id").(string)
	r := e.do("POST", "/v1/instances", "bob", small("intruder"), map[string]string{"X-Organization-ID": aliceOrg})
	if r.status != http.StatusForbidden {
		t.Errorf("bob creates in alice's org: %d", r.status)
	}
}

func TestNameTaken(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/instances", "alice", nano("db"), nil)
	if r := e.do("POST", "/v1/instances", "alice", nano("db"), nil); r.status != http.StatusConflict {
		t.Fatalf("duplicate name: %d %v", r.status, r.body)
	}
}

func TestQuotaSequential(t *testing.T) {
	e := newEnv(t)
	// small: 500m/1GiB/10GiB. Two fit the default 1 CPU / 2 GiB / 20 GiB / 2 instances.
	for _, n := range []string{"a", "b"} {
		if r := e.do("POST", "/v1/instances", "alice", small(n), nil); r.status != 202 {
			t.Fatalf("%s: %d %v", n, r.status, r.body)
		}
	}
	r := e.do("POST", "/v1/instances", "alice", nano("c"), nil)
	if r.status != http.StatusUnprocessableEntity || field(r.body, "error", "code") != "quota_exceeded" {
		t.Fatalf("third: %d %v", r.status, r.body)
	}
}

func TestConcurrentCreatesCannotBypassQuota(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "/v1/me", "alice", nil, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.do("POST", "/v1/instances", "alice", nano("db-"+string(rune('a'+i))), nil)
			mu.Lock()
			statuses[r.status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if statuses[202] != 2 || statuses[422] != 10 {
		t.Fatalf("statuses %v, want 2×202 and 10×422", statuses)
	}
	if e.count("instances") != 2 {
		t.Fatalf("%d instances", e.count("instances"))
	}
}

func TestCreateIsAtomic(t *testing.T) {
	e := newEnv(t)
	// Make the second insert of the transaction fail.
	_, err := e.pool.Exec(context.Background(), `
		CREATE FUNCTION fail_op() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
		CREATE TRIGGER fail_op BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION fail_op();`)
	if err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/v1/instances", "alice", small("db"), map[string]string{"Idempotency-Key": "k"})
	if r.status != http.StatusInternalServerError {
		t.Fatalf("status %d", r.status)
	}
	if e.count("instances") != 0 || e.count("idempotency_keys") != 0 {
		t.Fatal("failed create left rows behind")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}

func TestPlans(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/v1/plans", "alice", nil, nil)
	if r.status != 200 || len(r.body["plans"].([]any)) != 2 || r.body["default_postgres_version"] != "17" {
		t.Fatalf("plans: %v", r.body)
	}
}

func TestDeleteInstance(t *testing.T) {
	e := newEnv(t)
	a := e.do("POST", "/v1/instances", "alice", small("db"), nil)
	id := field(a.body, "instance", "id").(string)

	if r := e.do("DELETE", "/v1/instances/"+id, "bob", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("bob deletes alice's instance: %d", r.status)
	}
	d := e.do("DELETE", "/v1/instances/"+id, "alice", nil, nil)
	if d.status != http.StatusAccepted || field(d.body, "instance", "status") != "DELETING" {
		t.Fatalf("delete: %d %v", d.status, d.body)
	}
	again := e.do("DELETE", "/v1/instances/"+id, "alice", nil, nil)
	if again.status != http.StatusAccepted || field(again.body, "operation", "id") != field(d.body, "operation", "id") {
		t.Fatalf("repeat delete: %d %v", again.status, again.body)
	}
	g := e.do("GET", "/v1/instances/"+id, "alice", nil, nil)
	if g.body["desired_state"] != "DELETED" || g.body["status"] != "DELETING" {
		t.Fatalf("after delete: %v", g.body)
	}
	ops := e.do("GET", "/v1/instances/"+id+"/operations", "alice", nil, nil).body["operations"].([]any)
	if len(ops) != 2 || ops[0].(map[string]any)["action"] != "DELETE" || ops[1].(map[string]any)["status"] != "FAILED" {
		t.Fatalf("operations: %v", ops)
	}
	// Capacity stays reserved until the controller confirms deletion.
	e.do("POST", "/v1/instances", "alice", small("db2"), nil)
	if r := e.do("POST", "/v1/instances", "alice", nano("db3"), nil); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("quota released before deletion finished: %d", r.status)
	}
}

type fakeSecrets struct{}

func (fakeSecrets) AppCredentials(context.Context, string, string) (string, string, error) {
	return "app", "s3cret/+pw", nil
}
func (fakeSecrets) CACert(context.Context, string, string) ([]byte, error) {
	return []byte("-----BEGIN CERTIFICATE-----\n..."), nil
}

func TestConnectionAndCredentials(t *testing.T) {
	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.New(repository.New(pool), auth.Dev(), api.Config{}, logger).WithSecrets(fakeSecrets{}).Routes()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e := &env{t: t, pool: pool, srv: srv}

	id := field(e.do("POST", "/v1/instances", "alice", small("db"), nil).body, "instance", "id").(string)
	if r := e.do("GET", "/v1/instances/"+id+"/credentials", "alice", nil, nil); r.status != http.StatusConflict {
		t.Fatalf("credentials before ready: %d", r.status)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE instances SET observed_status = 'RUNNING',
		endpoint_host = 'pg-x.db.example.internal', endpoint_port = 5432, endpoint_address = '10.0.0.220' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	c := e.do("GET", "/v1/instances/"+id+"/connection", "alice", nil, nil)
	if c.status != 200 || c.body["sslmode"] != "verify-full" || c.body["password"] != nil ||
		c.body["psql"] != `psql "host=pg-x.db.example.internal port=5432 dbname=app user=app sslmode=verify-full hostaddr=10.0.0.220 sslrootcert=db-ca.crt"` {
		t.Fatalf("connection: %d %v", c.status, c.body)
	}
	cr := e.do("GET", "/v1/instances/"+id+"/credentials", "alice", nil, nil)
	if cr.status != 200 || cr.body["password"] != "s3cret/+pw" || cr.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("credentials: %d %v %v", cr.status, cr.body, cr.header)
	}
	if want := "postgresql://app:s3cret%2F+pw@pg-x.db.example.internal:5432/app?hostaddr=10.0.0.220&sslmode=verify-full&sslrootcert=db-ca.crt"; cr.body["uri"] != want {
		t.Errorf("uri %v", cr.body["uri"])
	}
	for _, p := range []string{"/credentials", "/connection"} {
		if r := e.do("GET", "/v1/instances/"+id+p, "bob", nil, nil); r.status != http.StatusNotFound || r.body["password"] != nil {
			t.Errorf("bob %s: %d", p, r.status)
		}
	}
	// Never in list or detail responses.
	for _, path := range []string{"/v1/instances", "/v1/instances/" + id} {
		raw := e.do("GET", path, "alice", nil, nil)
		if b, _ := json.Marshal(raw.body); bytes.Contains(b, []byte("s3cret")) {
			t.Errorf("%s leaks the password", path)
		}
	}
}

func TestCSRFGuard(t *testing.T) {
	e := newEnv(t)
	for name, h := range map[string]map[string]string{
		"form post":    {"Content-Type": "application/x-www-form-urlencoded"},
		"cross origin": {"Origin": "https://evil.example"},
	} {
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/instances", bytes.NewReader([]byte(`{"name":"db","plan":"nano"}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dev-User", "alice")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnsupportedMediaType && res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	if e.count("instances") != 0 {
		t.Fatal("forged request created an instance")
	}
	same := e.do("POST", "/v1/instances", "alice", nano("db"), map[string]string{"Origin": e.srv.URL})
	if same.status != http.StatusAccepted {
		t.Fatalf("same-origin: %d", same.status)
	}
}
