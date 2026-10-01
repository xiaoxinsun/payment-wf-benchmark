// Package cbs simulates the Core Bank System: accounts, an idempotent ledger and fault injection.
package cbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bill/ibps-bench/sims/simx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  acct text PRIMARY KEY, name text NOT NULL, status text NOT NULL, ccy text NOT NULL,
  balance_fen bigint NOT NULL DEFAULT 0, kind text NOT NULL);
CREATE TABLE IF NOT EXISTS postings (
  posting_ref text PRIMARY KEY, amount_fen bigint NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS legs (
  id bigserial PRIMARY KEY, posting_ref text NOT NULL, account text NOT NULL, signed_fen bigint NOT NULL);
CREATE INDEX IF NOT EXISTS legs_ref ON legs (posting_ref);
`

// Server is the CBS simulator.
type Server struct {
	Pool *pgxpool.Pool
	Sim  *simx.Sim
	// Call counters per endpoint group, so tests can prove "no external call was made" (B3, B4).
	Validates, Posts, StatusQueries atomic.Int64
}

// Open connects and ensures the schema exists.
func Open(ctx context.Context, url string, sim *simx.Sim) (*Server, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Server{Pool: pool, Sim: sim}, nil
}

// AccountID is the deterministic 16-digit account number for seed index i.
func AccountID(i int) string { return strconv.FormatInt(6222000000000000+int64(i), 10) }

// AccountName is the deterministic holder name for seed index i.
func AccountName(i int) string { return fmt.Sprintf("CUSTOMER %08d", i) }

// StatusForSuffix encodes the plan's test-data convention on the last four digits.
// An empty status means the account does not exist.
func StatusForSuffix(suffix int) string {
	switch {
	case suffix >= 9000 && suffix <= 9099:
		return ""
	case suffix >= 9100 && suffix <= 9199:
		return "CLOSED"
	case suffix >= 9200 && suffix <= 9299:
		return "FROZEN"
	case suffix >= 9300 && suffix <= 9399:
		return "DORMANT"
	}
	return "ACTIVE"
}

// Seed (re)creates n customer accounts and up to 64 settlement sub-accounts. Idempotent.
func (s *Server) Seed(ctx context.Context, n int) error {
	_, err := s.Pool.Exec(ctx, `
	INSERT INTO accounts (acct, name, status, ccy, kind)
	SELECT (6222000000000000 + i)::text, 'CUSTOMER ' || lpad(i::text, 8, '0'),
	  CASE WHEN i % 10000 BETWEEN 9100 AND 9199 THEN 'CLOSED'
	       WHEN i % 10000 BETWEEN 9200 AND 9299 THEN 'FROZEN'
	       WHEN i % 10000 BETWEEN 9300 AND 9399 THEN 'DORMANT' ELSE 'ACTIVE' END,
	  'CNY', 'CUSTOMER'
	FROM generate_series(0, $1 - 1) i
	WHERE NOT (i % 10000 BETWEEN 9000 AND 9099)
	ON CONFLICT DO NOTHING`, n)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `
	INSERT INTO accounts (acct, name, status, ccy, kind)
	SELECT 'SETTLE-IBPS-' || i, 'IBPS SETTLEMENT ' || i, 'ACTIVE', 'CNY', 'SETTLE' FROM generate_series(0, 63) i
	ON CONFLICT DO NOTHING`)
	return err
}

// Reset clears the ledger and zeroes balances (harness, between runs).
func (s *Server) Reset(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, `TRUNCATE postings, legs`); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE accounts SET balance_fen = 0 WHERE balance_fen <> 0`)
	return err
}

// Routes registers every endpoint on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	s.Sim.Control(mux)
	mux.HandleFunc("GET /accounts/{acct}/validate", s.validate)
	mux.HandleFunc("POST /postings", s.post)
	mux.HandleFunc("GET /postings/{ref}", s.getPosting)
	mux.HandleFunc("GET /ledger/invariants", s.invariants)
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		simx.JSON(w, http.StatusOK, map[string]int64{"validates": s.Validates.Load(), "posts": s.Posts.Load(), "statusQueries": s.StatusQueries.Load()})
	})
	mux.HandleFunc("POST /admin/reset", func(w http.ResponseWriter, r *http.Request) {
		s.Validates.Store(0)
		s.Posts.Store(0)
		s.StatusQueries.Store(0)
		if err := s.Reset(r.Context()); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /admin/seed", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("accounts"))
		if n <= 0 {
			n = 50000
		}
		if err := s.Seed(r.Context(), n); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

var statusReasons = map[string]string{"CLOSED": "CLOSED", "FROZEN": "FROZEN", "DORMANT": "DORMANT"}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	s.Validates.Add(1)
	s.Sim.Sleep()
	if f := s.Sim.Pick("validate"); f != nil {
		switch f.Mode {
		case "http503":
			http.Error(w, "injected", http.StatusServiceUnavailable)
			return
		case "latency":
			time.Sleep(time.Duration(f.LatencyMs) * time.Millisecond)
		}
	}
	var name, status string
	err := s.Pool.QueryRow(r.Context(), `SELECT name, status FROM accounts WHERE acct=$1 AND kind='CUSTOMER'`, r.PathValue("acct")).Scan(&name, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		simx.JSON(w, http.StatusUnprocessableEntity, map[string]string{"reason": "ACCOUNT_NOT_FOUND"})
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case statusReasons[status] != "":
		simx.JSON(w, http.StatusUnprocessableEntity, map[string]string{"reason": statusReasons[status]})
	case name != r.URL.Query().Get("name"):
		simx.JSON(w, http.StatusUnprocessableEntity, map[string]string{"reason": "NAME_MISMATCH"})
	default:
		simx.JSON(w, http.StatusOK, map[string]string{"status": "VALID"})
	}
}

type leg struct {
	Account   string `json:"account"`
	Side      string `json:"side"`
	AmountFen int64  `json:"amountFen"`
}

type postReq struct {
	PostingRef string `json:"postingRef"`
	Legs       []leg  `json:"legs"`
}

func (p postReq) balanced() (debit, credit leg, ok bool) {
	if p.PostingRef == "" || len(p.Legs) != 2 {
		return
	}
	a, b := p.Legs[0], p.Legs[1]
	if a.Side == "C" {
		a, b = b, a
	}
	if a.Side != "D" || b.Side != "C" || a.AmountFen <= 0 || a.AmountFen != b.AmountFen || a.Account == b.Account {
		return
	}
	return a, b, true
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	s.Posts.Add(1)
	s.Sim.Sleep()
	var req postReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	debit, credit, ok := req.balanced()
	if !ok {
		simx.JSON(w, http.StatusConflict, map[string]string{"status": "UNBALANCED"})
		return
	}
	fault := s.Sim.Pick("post")
	if fault != nil {
		switch fault.Mode {
		case "http503":
			http.Error(w, "injected", http.StatusServiceUnavailable)
			return
		case "timeout_before_commit": // I5: hang past the client timeout, then abandon without committing
			d := time.Duration(fault.LatencyMs) * time.Millisecond
			if d <= 0 {
				d = 1500 * time.Millisecond
			}
			select {
			case <-time.After(d):
			case <-r.Context().Done():
			}
			http.Error(w, "abandoned", http.StatusServiceUnavailable)
			return
		}
	}
	postedAt, already, err := s.commit(r.Context(), req.PostingRef, debit, credit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if fault != nil && fault.Mode == "commit_then_drop" { // I4: committed, but the caller never hears back
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	code, status := http.StatusCreated, "POSTED"
	if already {
		code, status = http.StatusOK, "ALREADY_POSTED"
	}
	simx.JSON(w, code, map[string]any{"status": status, "postedAt": postedAt})
}

// commit writes the posting in one transaction, unique on postingRef.
func (s *Server) commit(ctx context.Context, ref string, debit, credit leg) (postedAt time.Time, already bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO postings (posting_ref, amount_fen) VALUES ($1,$2) ON CONFLICT DO NOTHING`, ref, debit.AmountFen)
	if err != nil {
		return
	}
	if tag.RowsAffected() == 0 {
		already = true
		err = tx.QueryRow(ctx, `SELECT created_at FROM postings WHERE posting_ref=$1`, ref).Scan(&postedAt)
		return
	}
	// Settlement (hot) row first, then the customer row: one consistent lock order.
	if _, err = tx.Exec(ctx, `UPDATE accounts SET balance_fen = balance_fen - $2 WHERE acct=$1`, debit.Account, debit.AmountFen); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE accounts SET balance_fen = balance_fen + $2 WHERE acct=$1`, credit.Account, credit.AmountFen); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO legs (posting_ref, account, signed_fen) VALUES ($1,$2,$3),($1,$4,$5)`,
		ref, debit.Account, -debit.AmountFen, credit.Account, credit.AmountFen); err != nil {
		return
	}
	if err = tx.QueryRow(ctx, `SELECT created_at FROM postings WHERE posting_ref=$1`, ref).Scan(&postedAt); err != nil {
		return
	}
	err = tx.Commit(ctx)
	return
}

func (s *Server) getPosting(w http.ResponseWriter, r *http.Request) {
	s.StatusQueries.Add(1)
	s.Sim.Sleep()
	var at time.Time
	err := s.Pool.QueryRow(r.Context(), `SELECT created_at FROM postings WHERE posting_ref=$1`, r.PathValue("ref")).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		simx.JSON(w, http.StatusNotFound, map[string]string{"status": "NOT_FOUND"})
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		simx.JSON(w, http.StatusOK, map[string]any{"status": "POSTED", "postedAt": at})
	}
}

// Invariants is the ledger check the harness runs after every run.
type Invariants struct {
	LegSum               int64 `json:"legSum"`
	Postings             int64 `json:"postings"`
	Legs                 int64 `json:"legs"`
	PostingsNotTwoLegs   int64 `json:"postingsNotTwoLegs"`
	DuplicateRefs        int64 `json:"duplicateRefs"`
	SettlementBalanceFen int64 `json:"settlementBalanceFen"`
	CustomerBalanceFen   int64 `json:"customerBalanceFen"`
	BalancesReconcile    bool  `json:"balancesReconcile"`
	Ok                   bool  `json:"ok"`
}

func (s *Server) invariants(w http.ResponseWriter, r *http.Request) {
	var inv Invariants
	q := func(sql string, dst *int64) bool {
		if err := s.Pool.QueryRow(r.Context(), sql).Scan(dst); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return false
		}
		return true
	}
	ok := q(`SELECT COALESCE(SUM(signed_fen),0)::bigint FROM legs`, &inv.LegSum) &&
		q(`SELECT count(*) FROM postings`, &inv.Postings) &&
		q(`SELECT count(*) FROM legs`, &inv.Legs) &&
		q(`SELECT count(*) FROM (SELECT posting_ref FROM legs GROUP BY posting_ref HAVING count(*) <> 2) x`, &inv.PostingsNotTwoLegs) &&
		q(`SELECT count(*) - count(DISTINCT posting_ref) FROM postings`, &inv.DuplicateRefs) &&
		q(`SELECT COALESCE(SUM(balance_fen),0)::bigint FROM accounts WHERE kind='SETTLE'`, &inv.SettlementBalanceFen) &&
		q(`SELECT COALESCE(SUM(balance_fen),0)::bigint FROM accounts WHERE kind='CUSTOMER'`, &inv.CustomerBalanceFen)
	if !ok {
		return
	}
	inv.BalancesReconcile = inv.SettlementBalanceFen+inv.CustomerBalanceFen == 0
	inv.Ok = inv.LegSum == 0 && inv.DuplicateRefs == 0 && inv.PostingsNotTwoLegs == 0 &&
		inv.Legs == 2*inv.Postings && inv.BalancesReconcile
	simx.JSON(w, http.StatusOK, inv)
}

// SplitSuffix returns the last four digits of an account number as an int (used by tests and the NPC).
func SplitSuffix(acct string) int {
	if len(acct) < 4 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(acct[len(acct)-4:]))
	if err != nil {
		return -1
	}
	return n
}
