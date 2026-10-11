package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/igomez10/kindergarten/internal/controller"
	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/provisioner"
	"github.com/igomez10/kindergarten/internal/repository"
	"github.com/igomez10/kindergarten/internal/testdb"
)

// fakeProv records calls and returns scripted results.
type fakeProv struct {
	mu          sync.Mutex
	applies     int
	applyErr    error
	observe     provisioner.ObservedState
	deletes     int
	deleteOK    bool
	duringApply func()
}

func (f *fakeProv) Apply(context.Context, domain.Instance) error {
	f.mu.Lock()
	f.applies++
	hook := f.duringApply
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.applyErr
}
func (f *fakeProv) Observe(context.Context, domain.Instance) (provisioner.ObservedState, error) {
	return f.observe, nil
}
func (f *fakeProv) Delete(context.Context, domain.Instance) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return f.deleteOK, nil
}

type fixture struct {
	repo *repository.Repo
	org  uuid.UUID
	t    *testing.T
}

func setup(t *testing.T) fixture {
	pool := testdb.New(t)
	repo := repository.New(pool)
	_, ms, err := repo.EnsureUser(context.Background(), "dev", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{repo: repo, org: ms[0].Organization.ID, t: t}
}

func (fx fixture) create(name string) domain.Instance {
	req, plan, err := domain.CreateRequest{Name: name, Plan: "nano"}.Normalize()
	if err != nil {
		fx.t.Fatal(err)
	}
	res, err := fx.repo.CreateInstance(context.Background(), repository.CreateParams{
		OrganizationID: fx.org, Request: req, Plan: plan, K8sNamespace: "platform-test"})
	if err != nil {
		fx.t.Fatal(err)
	}
	return res.Instance
}

func (fx fixture) get(id uuid.UUID) domain.Instance {
	i, err := fx.repo.GetInstance(context.Background(), id, []uuid.UUID{fx.org})
	if err != nil {
		fx.t.Fatal(err)
	}
	return i
}

func newCtrl(fx fixture, p provisioner.Provisioner, owner string) *controller.Controller {
	return controller.New(fx.repo, p, controller.Config{Owner: owner, LeaseDuration: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func claimOne(t *testing.T, fx fixture, owner string) domain.Instance {
	t.Helper()
	insts, err := fx.repo.ClaimDue(context.Background(), owner, time.Minute, 10)
	if err != nil || len(insts) != 1 {
		t.Fatalf("claim: %v %d", err, len(insts))
	}
	return insts[0]
}

func TestReconcileToRunning(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	host, port := "pg.svc", 5432
	p := &fakeProv{observe: provisioner.ObservedState{Status: domain.StatusProvisioning, Message: "Creating the database."}}
	c := newCtrl(fx, p, "w1")

	c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
	got := fx.get(inst.ID)
	if got.ObservedStatus != domain.StatusProvisioning || got.AppliedGeneration != 1 || got.LeaseOwner != nil {
		t.Fatalf("after first pass: %s applied=%d lease=%v", got.ObservedStatus, got.AppliedGeneration, got.LeaseOwner)
	}
	if d := time.Until(got.NextReconcileAt); d < 3*time.Second || d > 6*time.Second {
		t.Errorf("PROVISIONING rescheduled in %v", d)
	}

	// Not due yet: nothing to claim.
	if insts, _ := fx.repo.ClaimDue(context.Background(), "w1", time.Minute, 10); len(insts) != 0 {
		t.Fatal("claimed an instance before it was due")
	}

	p.observe = provisioner.ObservedState{Status: domain.StatusRunning, EndpointHost: &host, EndpointPort: &port}
	got.NextReconcileAt = time.Now()
	forceDue(t, fx, inst.ID)
	c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
	got = fx.get(inst.ID)
	if got.ObservedStatus != domain.StatusRunning || *got.EndpointHost != "pg.svc" || got.StatusMessage != nil {
		t.Fatalf("after second pass: %+v", got)
	}
	ops, _ := fx.repo.ListOperations(context.Background(), inst.ID)
	if ops[0].Status != domain.OpSucceeded || ops[0].CompletedAt == nil {
		t.Errorf("create operation %s", ops[0].Status)
	}
}

func forceDue(t *testing.T, fx fixture, id uuid.UUID) {
	t.Helper()
	if err := fx.repo.ForceDue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseIsExclusive(t *testing.T) {
	fx := setup(t)
	fx.create("db")
	a, _ := fx.repo.ClaimDue(context.Background(), "w1", time.Minute, 10)
	b, _ := fx.repo.ClaimDue(context.Background(), "w2", time.Minute, 10)
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("w1 got %d, w2 got %d", len(a), len(b))
	}
}

func TestExpiredLeaseIsReclaimedAndFenced(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	stale, _ := fx.repo.ClaimDue(context.Background(), "w1", time.Millisecond, 10)
	time.Sleep(20 * time.Millisecond)
	fresh := claimOne(t, fx, "w2")
	if fresh.LeaseToken != stale[0].LeaseToken+1 {
		t.Fatalf("tokens %d -> %d", stale[0].LeaseToken, fresh.LeaseToken)
	}
	// The worker whose lease expired finishes late: its write must be rejected.
	saved, err := fx.repo.SaveOutcome(context.Background(), stale[0], repository.Outcome{Status: domain.StatusError, NextIn: time.Hour})
	if err != nil || saved {
		t.Fatalf("stale save accepted: %v %v", saved, err)
	}
	if got := fx.get(inst.ID); got.ObservedStatus != domain.StatusPending || *got.LeaseOwner != "w2" {
		t.Fatalf("state after stale save: %s %v", got.ObservedStatus, *got.LeaseOwner)
	}
}

func TestStaleGenerationIsNotSaved(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	p := &fakeProv{observe: provisioner.ObservedState{Status: domain.StatusRunning}}
	// While the worker is talking to Kubernetes, the desired state changes.
	p.duringApply = func() {
		if err := fx.repo.BumpGeneration(context.Background(), inst.ID); err != nil {
			t.Error(err)
		}
	}
	newCtrl(fx, p, "w1").Reconcile(context.Background(), claimOne(t, fx, "w1"))
	got := fx.get(inst.ID)
	if got.ObservedStatus != domain.StatusPending || got.AppliedGeneration != 0 || got.Generation != 2 {
		t.Fatalf("stale result written: %s applied=%d gen=%d", got.ObservedStatus, got.AppliedGeneration, got.Generation)
	}
	if got.LeaseOwner != nil || time.Until(got.NextReconcileAt) > time.Second {
		t.Fatal("lease not released / not requeued immediately")
	}
}

func TestRetryableFailureBacksOff(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	p := &fakeProv{applyErr: errors.New("connection refused")}
	c := newCtrl(fx, p, "w1")
	var delays []time.Duration
	for range 4 {
		forceDue(t, fx, inst.ID)
		c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
		got := fx.get(inst.ID)
		delays = append(delays, time.Until(got.NextReconcileAt))
		if got.ObservedStatus != domain.StatusProvisioning || got.LastError == nil {
			t.Fatalf("status %s err %v", got.ObservedStatus, got.LastError)
		}
	}
	if fx.get(inst.ID).ErrorCount != 4 || delays[3] < delays[0] {
		t.Fatalf("no backoff: %v", delays)
	}
	// Recovery clears the error.
	p.applyErr = nil
	p.observe = provisioner.ObservedState{Status: domain.StatusRunning}
	forceDue(t, fx, inst.ID)
	c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
	if got := fx.get(inst.ID); got.LastError != nil || got.ErrorCount != 0 || got.ObservedStatus != domain.StatusRunning {
		t.Fatalf("after recovery: %+v", got)
	}
}

func TestPermanentFailure(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	p := &fakeProv{applyErr: &provisioner.PermanentError{Message: "conflict"}}
	newCtrl(fx, p, "w1").Reconcile(context.Background(), claimOne(t, fx, "w1"))
	got := fx.get(inst.ID)
	if got.ObservedStatus != domain.StatusError || *got.LastError != "conflict" || time.Until(got.NextReconcileAt) < time.Minute {
		t.Fatalf("%+v", got)
	}
	ops, _ := fx.repo.ListOperations(context.Background(), inst.ID)
	if ops[0].Status != domain.OpFailed {
		t.Errorf("op %s", ops[0].Status)
	}
}

func TestRunLoopProvisionsViaNotify(t *testing.T) {
	fx := setup(t)
	p := &fakeProv{observe: provisioner.ObservedState{Status: domain.StatusRunning}}
	c := controller.New(fx.repo, p, controller.Config{Owner: "w1", PollInterval: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Listen(ctx, fx.repo.Pool())
	go c.Run(ctx)
	time.Sleep(300 * time.Millisecond) // LISTEN established, first scan done
	inst := fx.create("db")
	deadline := time.Now().Add(5 * time.Second)
	for fx.get(inst.ID).ObservedStatus != domain.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("NOTIFY did not wake the loop (poll interval is an hour)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDeletionWinsAndReleasesQuota(t *testing.T) {
	fx := setup(t)
	inst := fx.create("db")
	p := &fakeProv{observe: provisioner.ObservedState{Status: domain.StatusProvisioning}}
	// Provisioning is in flight when the delete arrives.
	p.duringApply = func() {
		if _, _, err := fx.repo.RequestDelete(context.Background(), inst.ID, []uuid.UUID{fx.org}); err != nil {
			t.Error(err)
		}
	}
	c := newCtrl(fx, p, "w1")
	c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
	if got := fx.get(inst.ID); got.ObservedStatus != domain.StatusDeleting {
		t.Fatalf("provisioning result overwrote the delete: %s", got.ObservedStatus)
	}
	p.duringApply = nil

	c.Reconcile(context.Background(), claimOne(t, fx, "w1")) // cleanup not finished
	if got := fx.get(inst.ID); got.ObservedStatus != domain.StatusDeleting || got.DeletedAt != nil {
		t.Fatalf("%s %v", got.ObservedStatus, got.DeletedAt)
	}
	p.deleteOK = true
	forceDue(t, fx, inst.ID)
	c.Reconcile(context.Background(), claimOne(t, fx, "w1"))
	got := fx.get(inst.ID)
	if got.ObservedStatus != domain.StatusDeleted || got.DeletedAt == nil {
		t.Fatalf("%s %v", got.ObservedStatus, got.DeletedAt)
	}
	ops, _ := fx.repo.ListOperations(context.Background(), inst.ID)
	if ops[0].Action != domain.OpDelete || ops[0].Status != domain.OpSucceeded {
		t.Fatalf("delete op %s %s", ops[0].Action, ops[0].Status)
	}
	// Name and quota are free again; deleted instances are never claimed.
	fx.create("db")
	if u, _ := fx.repo.Usage(context.Background(), fx.org); u.Instances != 1 {
		t.Fatalf("usage %+v", u)
	}
}

func TestBackoffBounds(t *testing.T) {
	for n := 1; n <= 20; n++ {
		for range 50 {
			d := controller.Backoff(n)
			if d < 1600*time.Millisecond || d > 60*time.Second {
				t.Fatalf("Backoff(%d) = %v", n, d)
			}
		}
	}
}
