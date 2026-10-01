package steps

import (
	"context"
	"errors"
	"time"

	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/obs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the payment-state table in app-db (engine-neutral; identical in every variant).
type Store struct{ Pool *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS payments (
  msg_id      text NOT NULL,
  e2e_id      text NOT NULL,
  state       text NOT NULL,
  rank        int  NOT NULL,
  reason      text NOT NULL DEFAULT '',
  received_at timestamptz NOT NULL,
  decided_at  timestamptz,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (msg_id, e2e_id)
)`

// OpenStore connects to app-db and ensures the schema exists.
func OpenStore(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Register inserts the payment as RECEIVED and reports whether this delivery was the first one.
func (s *Store) Register(ctx context.Context, p model.Payment, receivedAt time.Time) (first bool, err error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO payments (msg_id, e2e_id, state, rank, received_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, p.MsgID, p.EndToEndID, model.StateReceived, model.StateReceived.Rank(), receivedAt)
	return tag.RowsAffected() == 1, err
}

// Snapshot is the stored state of a payment, returned to duplicate deliveries.
type Snapshot struct {
	State  model.PaymentState
	Reason model.RejectReason
}

// Get returns the stored state, or ok=false if the payment is unknown.
func (s *Store) Get(ctx context.Context, p model.Payment) (Snapshot, bool, error) {
	var st, rs string
	err := s.Pool.QueryRow(ctx, `SELECT state, reason FROM payments WHERE msg_id=$1 AND e2e_id=$2`, p.MsgID, p.EndToEndID).Scan(&st, &rs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	return Snapshot{State: model.PaymentState(st), Reason: model.RejectReason(rs)}, err == nil, err
}

// RecordState advances the payment to state. It is idempotent and never moves a payment backwards, so
// a retried or replayed step is harmless.
func (s *Store) RecordState(ctx context.Context, p model.Payment, state model.PaymentState, reason model.RejectReason, decidedAt time.Time) error {
	var decided any
	if !decidedAt.IsZero() {
		decided = decidedAt
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE payments SET state=$3, rank=$4, reason=$5, decided_at=COALESCE($6, decided_at), updated_at=now()
		WHERE msg_id=$1 AND e2e_id=$2 AND rank < $4`, p.MsgID, p.EndToEndID, state, state.Rank(), string(reason), decided)
	if err == nil && tag.RowsAffected() == 1 && state.Terminal() {
		obs.PaymentsTotal.WithLabelValues(string(state)).Inc()
	}
	return err
}

// Reset truncates the table (harness, between runs).
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `TRUNCATE payments`)
	return err
}

// RecordState on Deps is the step wrapper used by the engines.
func (d *Deps) RecordState(ctx context.Context, in model.PaymentInput, state model.PaymentState, reason model.RejectReason, decidedAt time.Time) error {
	defer obs.ObserveStep("record", time.Now())
	if err := d.Store.RecordState(ctx, in.Payment, state, reason, decidedAt); err != nil {
		return transient("record %s: %v", state, err)
	}
	return nil
}
