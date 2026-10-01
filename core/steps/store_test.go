package steps

import (
	"context"
	"testing"
	"time"

	"github.com/bill/ibps-bench/core/model"
)

// These tests need app-db (make up-v1 or up-v2). They skip when it is not reachable.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := OpenStore(ctx, "postgres://bench:bench@localhost:5435/app?sslmode=disable")
	if err != nil {
		t.Skipf("app-db not reachable: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := testPayment()

	if _, ok, err := s.Get(ctx, p); err != nil || ok {
		t.Fatalf("unknown payment: ok=%v err=%v", ok, err)
	}
	first, err := s.Register(ctx, p, time.Now())
	if err != nil || !first {
		t.Fatalf("first delivery: %v %v", first, err)
	}
	if again, err := s.Register(ctx, p, time.Now()); err != nil || again {
		t.Fatalf("duplicate delivery must not be first: %v %v", again, err)
	}

	steps := []model.PaymentState{model.StateValidated, model.StateScreened, model.StateSettled, model.StateAcceptedSent}
	for _, st := range steps {
		if err := s.RecordState(ctx, p, st, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	snap, ok, _ := s.Get(ctx, p)
	if !ok || snap.State != model.StateAcceptedSent {
		t.Fatalf("expected ACCEPTED_SENT, got %+v", snap)
	}
	// Replays and stale writes never move a payment backwards.
	for _, st := range []model.PaymentState{model.StateScreened, model.StateAcceptedSent, model.StateValidated} {
		if err := s.RecordState(ctx, p, st, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if snap, _, _ = s.Get(ctx, p); snap.State != model.StateAcceptedSent {
		t.Errorf("state moved backwards: %+v", snap)
	}
}

func TestStoreRejectPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := testPayment()
	p.MsgID = "M-rej"
	s.Register(ctx, p, time.Now())
	s.RecordState(ctx, p, model.StateRejected, model.ReasonAMLHit, time.Now())
	s.RecordState(ctx, p, model.StateRejectedSent, model.ReasonAMLHit, time.Time{})
	snap, _, _ := s.Get(ctx, p)
	if snap.State != model.StateRejectedSent || snap.Reason != model.ReasonAMLHit {
		t.Errorf("reject path: %+v", snap)
	}
}

func TestDepsRecordStateWrapper(t *testing.T) {
	s := testStore(t)
	d := newDeps("", "", "")
	d.Store = s
	in := input(time.Minute)
	in.Payment.MsgID = "M-wrap"
	s.Register(context.Background(), in.Payment, time.Now())
	if err := d.RecordState(context.Background(), in, model.StateValidated, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Pool.Close()
	if err := d.RecordState(context.Background(), in, model.StateScreened, "", time.Now()); err == nil {
		t.Error("closed pool must return an error")
	}
}
