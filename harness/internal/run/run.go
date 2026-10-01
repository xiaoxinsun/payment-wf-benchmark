// Package run executes one benchmark run end to end: reset, load, timed events, drain, invariant checks.
package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bill/ibps-bench/harness/internal/env"
	"github.com/bill/ibps-bench/sims/npc"
)

// Event is a timed action during a run (fault injection, kill, restart, ...).
type Event struct {
	AtS  float64
	Name string
	Do   func(ctx context.Context, e *env.Env) error
}

// Spec describes one run. Rate/Duration/Warmup are NPC-side; Events run on the harness clock from run start.
type Spec struct {
	Scenario  string
	Label     string
	Rate      float64
	DurationS int
	WarmupS   int
	DrainS    int
	Mix       map[string]float64
	Mode      string
	StepUp    *npc.StepUp
	Seed      uint64
	Shards    int                // settlement sub-accounts (0 = keep current)
	SimMs     map[string]float64 // cbs/aml/npc base latency (nil = plan baseline 5/10/3)
	Events    []Event            `json:"-"`
	Cooldown  time.Duration
	// Probe runs observe behaviour instead of asserting it: generic invariant failures become notes.
	Probe bool
	// Expect holds the fault-specific assertions, run after the generic invariants.
	Expect func(r *Result) []string `json:"-"`
}

// EventLog records when an event really ran.
type EventLog struct {
	Name  string  `json:"name"`
	AtMs  float64 `json:"atMs"`
	Error string  `json:"error,omitempty"`
}

type ResStat struct {
	MeanCores float64 `json:"meanCores"`
	MaxCores  float64 `json:"maxCores"`
	MaxMemMiB float64 `json:"maxMemMiB"`
}

type DBStats struct {
	XactCommit int64 `json:"xactCommit"`
	TupIns     int64 `json:"tupInserted"`
	TupUpd     int64 `json:"tupUpdated"`
	TupDel     int64 `json:"tupDeleted"`
	WalBytes   int64 `json:"walBytes"`
	WalRecords int64 `json:"walRecords"`
	StmtCalls  int64 `json:"stmtCalls"`
	StmtRows   int64 `json:"stmtRows"`
}

type Verdict struct {
	Pass     bool     `json:"pass"`
	Failures []string `json:"failures,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	// Counts behind the checks.
	Acked           int   `json:"acked"`
	StuckAfterDrain int   `json:"stuckAfterDrain"`
	Lost            int   `json:"lost"`
	Postings        int64 `json:"postings"`
	AcceptedSent    int   `json:"acceptedSent"`
	RejectedSent    int   `json:"rejectedSent"`
	UnackedRows     int   `json:"unackedRows"`
}

type Result struct {
	Variant   string             `json:"variant"`
	Spec      Spec               `json:"spec"`
	Rep       int                `json:"rep"`
	RunID     string             `json:"runId"`
	StartedAt time.Time          `json:"startedAt"`
	WallS     float64            `json:"wallS"`
	Summary   npc.Summary        `json:"summary"`
	Verdict   Verdict            `json:"verdict"`
	Events    []EventLog         `json:"events,omitempty"`
	DB        map[string]DBStats `json:"db"`
	PerPay    map[string]float64 `json:"perPayment"` // durability DB writes, WAL bytes, statements per payment
	Resources map[string]ResStat `json:"resources"`
	Ledger    json.RawMessage    `json:"ledger,omitempty"`
	// RecoveryS is the time from the last disruptive event until the last stalled payment was answered.
	RecoveryS float64 `json:"recoveryS,omitempty"`
	Aborted   string  `json:"aborted,omitempty"`
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}

// ---- reset -----------------------------------------------------------------------------------------

// Prepare brings the stack to a clean, identical state before a run.
func Prepare(ctx context.Context, e *env.Env, s Spec) error {
	if s.Shards > 0 && s.Shards != e.Shards {
		e.Shards = s.Shards
		if err := e.Recreate(ctx); err != nil {
			return err
		}
	}
	if err := e.ClearSimFaults(ctx); err != nil {
		return err
	}
	if err := e.ClearToxics(ctx); err != nil {
		return err
	}
	if _, err := e.Do(ctx, "POST", e.NPCURL+"/npc/reset", nil, nil); err != nil {
		return err
	}
	lat := map[string]float64{"cbs": 5, "aml": 10, "npc": 3}
	for k, v := range s.SimMs {
		lat[k] = v
	}
	for sim, ms := range lat {
		if err := e.SimLatency(ctx, sim, ms); err != nil {
			return err
		}
	}
	// Stop the replicas first so nothing writes while the tables are cleared.
	for _, r := range e.V.Replicas {
		if err := e.Kill(ctx, r); err != nil {
			logf("prepare: kill %s: %v", r, err)
		}
	}
	if code, err := e.Do(ctx, "POST", e.CBSURL+"/admin/reset", nil, nil); err != nil || code != 200 {
		return fmt.Errorf("cbs reset: %v (%d)", err, code)
	}
	app, err := e.DB(ctx, "app")
	if err != nil {
		return err
	}
	if _, err := app.Exec(ctx, `TRUNCATE payments`); err != nil {
		return err
	}
	if e.V.Engine == "temporal" {
		if err := e.ResetTemporal(ctx); err != nil {
			return err
		}
	}
	if e.V.Engine == "dbos" {
		d, err := e.DB(ctx, "dbos")
		if err != nil {
			return err
		}
		if _, err := d.Exec(ctx, `TRUNCATE dbos.workflow_status CASCADE`); err != nil {
			return err
		}
	}
	for _, db := range []string{"app", "cbs", e.DurabilityDB()} {
		p, err := e.DB(ctx, db)
		if err != nil {
			return err
		}
		if _, err := p.Exec(ctx, `CHECKPOINT`); err != nil {
			return err
		}
		if _, err := p.Exec(ctx, `SELECT pg_stat_statements_reset()`); err != nil {
			return err
		}
		if _, err := p.Exec(ctx, `SELECT pg_stat_reset_shared('wal')`); err != nil {
			return err
		}
	}
	return e.RestartReplicas(ctx)
}

// ---- db stats ----------------------------------------------------------------------------------------

func snapshot(ctx context.Context, e *env.Env, db string) (DBStats, error) {
	p, err := e.DB(ctx, db)
	if err != nil {
		return DBStats{}, err
	}
	var s DBStats
	if err := p.QueryRow(ctx, `SELECT COALESCE(sum(xact_commit+xact_rollback),0)::bigint, COALESCE(sum(tup_inserted),0)::bigint,
		COALESCE(sum(tup_updated),0)::bigint, COALESCE(sum(tup_deleted),0)::bigint FROM pg_stat_database`).Scan(&s.XactCommit, &s.TupIns, &s.TupUpd, &s.TupDel); err != nil {
		return s, err
	}
	if err := p.QueryRow(ctx, `SELECT wal_bytes::bigint, wal_records FROM pg_stat_wal`).Scan(&s.WalBytes, &s.WalRecords); err != nil {
		return s, err
	}
	err = p.QueryRow(ctx, `SELECT COALESCE(sum(calls),0)::bigint, COALESCE(sum(rows),0)::bigint FROM pg_stat_statements`).Scan(&s.StmtCalls, &s.StmtRows)
	return s, err
}

func diff(a, b DBStats) DBStats {
	return DBStats{b.XactCommit - a.XactCommit, b.TupIns - a.TupIns, b.TupUpd - a.TupUpd, b.TupDel - a.TupDel,
		b.WalBytes - a.WalBytes, b.WalRecords - a.WalRecords, b.StmtCalls - a.StmtCalls, b.StmtRows - a.StmtRows}
}

// ---- resource sampling -------------------------------------------------------------------------------

type sampler struct {
	mu      sync.Mutex
	samples []env.Sample
	stop    chan struct{}
	done    chan struct{}
}

func startSampler(ctx context.Context, e *env.Env) *sampler {
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			if got, err := e.SampleStats(ctx); err == nil {
				s.mu.Lock()
				s.samples = append(s.samples, got...)
				s.mu.Unlock()
			}
			select {
			case <-s.stop:
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
	return s
}

func (s *sampler) finish() map[string]ResStat {
	close(s.stop)
	<-s.done
	agg := map[string]*struct {
		sum, max, mem float64
		n             int
	}{}
	for _, x := range s.samples {
		a := agg[x.Name]
		if a == nil {
			a = &struct {
				sum, max, mem float64
				n             int
			}{}
			agg[x.Name] = a
		}
		a.sum += x.CPU
		a.n++
		if x.CPU > a.max {
			a.max = x.CPU
		}
		if x.MemMiB > a.mem {
			a.mem = x.MemMiB
		}
	}
	out := map[string]ResStat{}
	for name, a := range agg {
		out[name] = ResStat{MeanCores: a.sum / float64(a.n) / 100, MaxCores: a.max / 100, MaxMemMiB: a.mem}
	}
	return out
}

// ---- the run -------------------------------------------------------------------------------------------

// Once executes a full run and returns its result (never nil). Infrastructure errors are recorded in
// Result.Aborted; correctness failures in Result.Verdict.
func Once(ctx context.Context, e *env.Env, s Spec, rep int) *Result {
	res := &Result{Variant: e.V.Name, Spec: s, Rep: rep, StartedAt: time.Now(), DB: map[string]DBStats{}, PerPay: map[string]float64{}}
	fail := func(err error) *Result { res.Aborted = err.Error(); return res }
	logf("[%s] %s/%s rep %d: preparing", e.V.Name, s.Scenario, s.Label, rep)
	if err := Prepare(ctx, e, s); err != nil {
		return fail(fmt.Errorf("prepare: %w", err))
	}
	dbs := []string{"app", "cbs", e.DurabilityDB()}
	before := map[string]DBStats{}
	for _, d := range dbs {
		st, err := snapshot(ctx, e, d)
		if err != nil {
			return fail(fmt.Errorf("snapshot %s: %w", d, err))
		}
		before[d] = st
	}
	sm := startSampler(ctx, e)

	runID := fmt.Sprintf("r%s", time.Now().Format("0102150405"))
	req := npc.RunRequest{RunID: runID, Rate: s.Rate, DurationS: s.DurationS, WarmupS: s.WarmupS, DrainS: s.DrainS,
		Mode: s.Mode, IngressURL: "http://lb:80", Mix: s.Mix, Seed: s.Seed, StepUp: s.StepUp}
	t0 := time.Now()
	if code, err := e.Do(ctx, "POST", e.NPCURL+"/npc/run", req, nil); err != nil || code != http0202 {
		sm.finish()
		return fail(fmt.Errorf("start npc run: %v (%d)", err, code))
	}
	var wg sync.WaitGroup
	var evMu sync.Mutex
	for _, ev := range s.Events {
		wg.Add(1)
		go func(ev Event) {
			defer wg.Done()
			select {
			case <-time.After(time.Until(t0.Add(time.Duration(ev.AtS * float64(time.Second))))):
			case <-ctx.Done():
				return
			}
			at := time.Since(t0)
			err := ev.Do(ctx, e)
			l := EventLog{Name: ev.Name, AtMs: float64(at) / 1e6}
			if err != nil {
				l.Error = err.Error()
			}
			evMu.Lock()
			res.Events = append(res.Events, l)
			evMu.Unlock()
		}(ev)
	}
	total := time.Duration(s.WarmupS+s.DurationS+s.DrainS)*time.Second + 10*time.Minute
	if s.StepUp != nil {
		total = 3 * time.Hour
	}
	deadline := time.Now().Add(total)
	for {
		var st struct {
			State string `json:"state"`
		}
		if _, err := e.Do(ctx, "GET", e.NPCURL+"/npc/status", nil, &st); err == nil && st.State == "done" {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			wg.Wait()
			sm.finish()
			return fail(fmt.Errorf("npc run did not finish"))
		}
		time.Sleep(time.Second)
	}
	wg.Wait()
	res.Resources = sm.finish()
	res.WallS = time.Since(t0).Seconds()
	sort.Slice(res.Events, func(i, j int) bool { return res.Events[i].AtMs < res.Events[j].AtMs })

	if _, err := e.Do(ctx, "GET", e.NPCURL+"/npc/results", nil, &res.Summary); err != nil {
		return fail(fmt.Errorf("npc results: %w", err))
	}
	for _, d := range dbs {
		after, err := snapshot(ctx, e, d)
		if err != nil {
			return fail(fmt.Errorf("snapshot %s: %w", d, err))
		}
		res.DB[d] = diff(before[d], after)
	}
	if n := float64(res.Summary.Received); n > 0 {
		dur := res.DB[e.DurabilityDB()]
		res.PerPay["durabilityXacts"] = float64(dur.XactCommit) / n
		res.PerPay["durabilityRowsWritten"] = float64(dur.TupIns+dur.TupUpd+dur.TupDel) / n
		res.PerPay["durabilityWalBytes"] = float64(dur.WalBytes) / n
		res.PerPay["durabilityStatements"] = float64(dur.StmtCalls) / n
	}
	res.RunID = runID
	verify(ctx, e, res, runID)
	res.RecoveryS = recovery(res)
	if s.Expect != nil {
		for _, f := range s.Expect(res) {
			res.Verdict.Failures = append(res.Verdict.Failures, f)
		}
		res.Verdict.Pass = len(res.Verdict.Failures) == 0
	}
	// Probe runs observe behaviour instead of gating on it: apply this last so nothing (verify or Expect)
	// can re-fail the run afterwards.
	if s.Probe {
		for _, f := range res.Verdict.Failures {
			res.Verdict.Notes = append(res.Verdict.Notes, "observed: "+f)
		}
		res.Verdict.Failures, res.Verdict.Pass = nil, true
	}
	logf("[%s] %s/%s rep %d: done pass=%v sent=%d p50=%.0fms p99=%.0fms", e.V.Name, s.Scenario, s.Label, rep,
		res.Verdict.Pass, res.Summary.Sent, res.Summary.Latency.P50, res.Summary.Latency.P99)
	return res
}

const http0202 = 202

// recovery: time from the last disruptive event to the last stalled payment being answered.
func recovery(r *Result) float64 {
	if r.Summary.StallCount == 0 || len(r.Events) == 0 {
		return 0
	}
	last := 0.0
	for _, ev := range r.Events {
		if ev.AtMs > last {
			last = ev.AtMs
		}
	}
	rec := r.Summary.StallLastMs - last
	if rec < 0 {
		rec = 0
	}
	return rec / 1000
}

// ---- verification ---------------------------------------------------------------------------------------

type detailRec struct {
	Seq     int    `json:"seq"`
	AckCode int    `json:"ackCode"`
	RecvNs  int64  `json:"recvNs"`
	Cat     string `json:"cat"`
}

func fetchDetail(ctx context.Context, e *env.Env) ([]detailRec, error) {
	req, _ := newGet(ctx, e.NPCURL+"/npc/results?detail=1")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []detailRec
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var d detailRec
		if json.Unmarshal(sc.Bytes(), &d) == nil {
			out = append(out, d)
		}
	}
	return out, sc.Err()
}

// verify applies the plan's pass criteria. Every failure is one line in Verdict.Failures.
func verify(ctx context.Context, e *env.Env, res *Result, runID string) {
	v := &res.Verdict
	failf := func(format string, a ...any) { v.Failures = append(v.Failures, fmt.Sprintf(format, a...)) }
	app, err := e.DB(ctx, "app")
	if err != nil {
		failf("app-db: %v", err)
		return
	}
	cbs, err := e.DB(ctx, "cbs")
	if err != nil {
		failf("cbs-db: %v", err)
		return
	}
	detail, err := fetchDetail(ctx, e)
	if err != nil {
		failf("npc detail: %v", err)
		return
	}
	acked := map[string]bool{}
	for _, d := range detail {
		if d.AckCode == 202 || d.AckCode == 200 {
			acked[fmt.Sprintf("%s-%08d", runID, d.Seq)] = true
		}
	}
	v.Acked = len(acked)

	// Zero payments left in a non-terminal state 60 s after load stops (only payments the NPC saw acked).
	stuckSince := time.Now()
	var nonTerminal []string
	for {
		nonTerminal = nonTerminal[:0]
		rows, err := app.Query(ctx, `SELECT msg_id FROM payments WHERE rank < 5`)
		if err != nil {
			failf("app-db query: %v", err)
			return
		}
		for rows.Next() {
			var id string
			rows.Scan(&id)
			nonTerminal = append(nonTerminal, id)
		}
		rows.Close()
		var ackedStuck int
		for _, id := range nonTerminal {
			if acked[id] {
				ackedStuck++
			}
		}
		if ackedStuck == 0 || time.Since(stuckSince) > 60*time.Second {
			v.StuckAfterDrain = ackedStuck
			v.UnackedRows = len(nonTerminal) - ackedStuck
			break
		}
		time.Sleep(2 * time.Second)
	}
	if v.StuckAfterDrain > 0 {
		failf("%d acknowledged payments still non-terminal 60s after load stopped", v.StuckAfterDrain)
	}

	// Every acknowledged ibps.101 has exactly one row (nothing lost).
	present := map[string]bool{}
	rows, err := app.Query(ctx, `SELECT msg_id, state FROM payments`)
	if err != nil {
		failf("app-db query: %v", err)
		return
	}
	for rows.Next() {
		var id, st string
		rows.Scan(&id, &st)
		present[id] = true
		switch st {
		case "ACCEPTED_SENT":
			v.AcceptedSent++
		case "REJECTED_SENT":
			v.RejectedSent++
		}
	}
	rows.Close()
	for id := range acked {
		if !present[id] {
			v.Lost++
		}
	}
	if v.Lost > 0 {
		failf("%d acknowledged payments have no state row (lost)", v.Lost)
	}

	// Ledger invariants and posting/payment agreement.
	var raw json.RawMessage
	if code, err := e.Do(ctx, "GET", e.CBSURL+"/ledger/invariants", nil, &raw); err != nil || code != 200 {
		failf("ledger invariants: %v (%d)", err, code)
	} else {
		res.Ledger = raw
		var inv struct {
			Ok       bool  `json:"ok"`
			LegSum   int64 `json:"legSum"`
			Dups     int64 `json:"duplicateRefs"`
			Postings int64 `json:"postings"`
		}
		json.Unmarshal(raw, &inv)
		v.Postings = inv.Postings
		if !inv.Ok {
			failf("ledger invariants violated: %s", string(raw))
		}
	}
	if v.Postings != int64(v.AcceptedSent) {
		failf("postings (%d) != ACCEPTED_SENT payments (%d)", v.Postings, v.AcceptedSent)
	}
	// No posting exists for a rejected payment.
	rrows, err := app.Query(ctx, `SELECT msg_id, e2e_id FROM payments WHERE state IN ('REJECTED','REJECTED_SENT')`)
	if err == nil {
		var refs []string
		for rrows.Next() {
			var m, e2 string
			rrows.Scan(&m, &e2)
			refs = append(refs, "IBPS-102100099996-"+m+"-"+e2)
		}
		rrows.Close()
		for i := 0; i < len(refs); i += 5000 {
			end := min(i+5000, len(refs))
			var n int
			if err := cbs.QueryRow(ctx, `SELECT count(*) FROM postings WHERE posting_ref = ANY($1)`, refs[i:end]).Scan(&n); err == nil && n > 0 {
				failf("%d rejected payments have a posting", n)
			}
		}
	}
	// One ibps.102 per payment, all answered.
	s := res.Summary
	if s.DoubleAnswered > 0 {
		failf("%d payments received two different ibps.102 messages", s.DoubleAnswered)
	}
	if s.Unanswered > 0 {
		v.Notes = append(v.Notes, fmt.Sprintf("%d acknowledged payments unanswered at NPC after drain", s.Unanswered))
		failf("%d payments never received an ibps.102", s.Unanswered)
	}
	if s.DupBadAcks > 0 {
		failf("%d duplicate deliveries were not acknowledged normally", s.DupBadAcks)
	}
	if int64(s.Received) > 0 && s.Sent > 0 && v.AcceptedSent+v.RejectedSent < s.IngressOK {
		v.Notes = append(v.Notes, fmt.Sprintf("terminal rows %d < acknowledged %d", v.AcceptedSent+v.RejectedSent, s.IngressOK))
	}
	v.Pass = len(v.Failures) == 0
}

// ---- persistence ------------------------------------------------------------------------------------------

// Path is where a result is stored.
func Path(root, variant, scenario, label string, rep int) string {
	return filepath.Join(root, "results", variant, scenario, fmt.Sprintf("%s-rep%d.json", strings.ReplaceAll(label, "/", "_"), rep))
}

func Save(root string, r *Result) (string, error) {
	p := Path(root, r.Variant, r.Spec.Scenario, r.Spec.Label, r.Rep)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return "", err
	}
	return p, os.WriteFile(p, b, 0o644)
}

func Load(path string) (*Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	return &r, json.Unmarshal(b, &r)
}
