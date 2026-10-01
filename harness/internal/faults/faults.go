// Package faults is the fault catalogue: 8 business faults (data-driven), 12 infrastructure faults and the
// optional C1 code-fault probe. Every fault has an injection and one asserted expected outcome.
package faults

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bill/ibps-bench/harness/internal/env"
	"github.com/bill/ibps-bench/harness/internal/run"
)

// Fault is one catalogue entry.
type Fault struct {
	ID     string
	Desc   string
	V1Only bool
	Build  func(v env.Variant, rate float64) run.Spec
}

// Timing of every infrastructure fault run: steady load, fault at 6 s, recovery, then a tail. Shortened
// from the plan's 15s/50s (Bill, 2026-09-30: whole matrix under 3h, indicative not fully-fledged).
const (
	faultAtS  = 6.0
	durationS = 20
)

func base(id string, rate float64, mix map[string]float64) run.Spec {
	if mix == nil {
		mix = map[string]float64{"happy": 100}
	}
	return run.Spec{Scenario: "S4", Label: id, Rate: rate, DurationS: durationS, WarmupS: 0, DrainS: 20,
		Mix: mix, Seed: 42, Cooldown: 8 * time.Second}
}

func at(s float64, name string, do func(ctx context.Context, e *env.Env) error) run.Event {
	return run.Event{AtS: s, Name: name, Do: do}
}

func simFault(sim, target, mode string, rate float64, durMs, latMs int) func(context.Context, *env.Env) error {
	return func(ctx context.Context, e *env.Env) error {
		return e.SimFault(ctx, sim, map[string]any{"target": target, "mode": mode, "rate": rate, "durationMs": durMs, "latencyMs": latMs})
	}
}

func removeToxic(proxy, name string) func(context.Context, *env.Env) error {
	return func(ctx context.Context, e *env.Env) error {
		_, err := e.Do(ctx, "DELETE", e.ToxiURL+"/proxies/"+proxy+"/toxics/"+name, nil, nil)
		return err
	}
}

// ---- assertions ----------------------------------------------------------------------------------------

// onlyOutcomes fails if any final outcome is outside the allowed set.
func onlyOutcomes(r *run.Result, allowed ...string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var bad []string
	for o, n := range r.Summary.Outcomes {
		if !ok[o] {
			bad = append(bad, fmt.Sprintf("unexpected outcome %s x%d (allowed %v)", o, n, allowed))
		}
	}
	return bad
}

// onlyOutcomesTolerant is onlyOutcomes but tolerates up to maxRate of AB05 (SLA timeout), because on a
// shared laptop under injected load a rare payment can legitimately cross the 5s SLA during the fault
// window without any correctness defect. A systemic problem shows up as a rate far above this.
func onlyOutcomesTolerant(r *run.Result, maxRate float64, allowed ...string) []string {
	extra := append(append([]string{}, allowed...), "AB05")
	bad := onlyOutcomes(r, extra...)
	if len(bad) > 0 {
		return bad
	}
	n := r.Summary.Received
	if n == 0 {
		return nil
	}
	rate := float64(r.Summary.Outcomes["AB05"]) / float64(n)
	return need(rate <= maxRate, "AB05 rate %.2f%% (%d/%d) is above the %.2f%% tolerance for isolated SLA misses under load", rate*100, r.Summary.Outcomes["AB05"], n, maxRate*100)
}

// categoriesAsExpected fails on any payment whose outcome differs from its category's known outcome.
func categoriesAsExpected(r *run.Result) []string {
	var bad []string
	for name, c := range r.Summary.ByCategory {
		if c.Mismatch > 0 {
			bad = append(bad, fmt.Sprintf("category %s: %d of %d payments had an unexpected outcome %v", name, c.Mismatch, c.Sent, c.Outcomes))
		}
	}
	return bad
}

func rejectedNeverPost(r *run.Result) []string {
	if r.Summary.Received > 0 && r.Verdict.Postings != int64(r.Summary.Outcomes["ACCP"]) {
		return []string{fmt.Sprintf("postings %d != ACCP receipts %d", r.Verdict.Postings, r.Summary.Outcomes["ACCP"])}
	}
	return nil
}

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func need(cond bool, format string, a ...any) []string {
	if cond {
		return nil
	}
	return []string{fmt.Sprintf(format, a...)}
}

// cbsCalls reads the CBS call counters (proof that step 2 rejects made no external call).
func cbsCalls(ctx context.Context, e *env.Env) (validates, posts int64, err error) {
	var st map[string]int64
	if _, err = e.Do(ctx, "GET", e.CBSURL+"/stats", nil, &st); err != nil {
		return
	}
	return st["validates"], st["posts"], nil
}

// ---- catalogue ------------------------------------------------------------------------------------------

func dataFault(id, desc string, mix map[string]float64, extra func(r *run.Result) []string) Fault {
	return Fault{ID: id, Desc: desc, Build: func(v env.Variant, rate float64) run.Spec {
		s := base(id, rate, mix)
		s.DurationS = 15
		s.Expect = func(r *run.Result) []string {
			return join(categoriesAsExpected(r), rejectedNeverPost(r), need(r.Summary.Received > 0, "no payments completed"), extraOrNil(extra, r))
		}
		return s
	}}
}

func extraOrNil(f func(*run.Result) []string, r *run.Result) []string {
	if f == nil {
		return nil
	}
	return f(r)
}

func dup(id, desc, mode string) Fault {
	return Fault{ID: id, Desc: desc, Build: func(v env.Variant, rate float64) run.Spec {
		s := base(id, rate, nil)
		s.DurationS = 15
		s.Events = []run.Event{at(0, "duplicate deliveries "+mode, simFault("npc", "send", mode, 0.3, 0, 0))}
		s.Expect = func(r *run.Result) []string {
			return join(onlyOutcomes(r, "ACCP"),
				need(r.Summary.DupDeliveries > 0, "no duplicate was actually sent"),
				need(r.Summary.DoubleAnswered == 0 && r.Summary.MultiReceipts == 0, "duplicate produced %d double-answered / %d multi-receipt payments", r.Summary.DoubleAnswered, r.Summary.MultiReceipts),
				need(r.Verdict.Postings == int64(r.Summary.Sent), "postings %d != unique payments %d (one posting per payment)", r.Verdict.Postings, r.Summary.Sent))
		}
		return s
	}}
}

// Catalogue returns every fault in the plan's order.
func Catalogue() []Fault {
	f := []Fault{
		dataFault("B1", "Account not found, closed, frozen, dormant -> RJCT AC01/AC04/AC06, no posting",
			map[string]float64{"acct_not_found": 25, "acct_closed": 25, "acct_frozen": 25, "acct_dormant": 25}, nil),
		dataFault("B2", "Name mismatch -> RJCT BE01, no posting", map[string]float64{"name_mismatch": 100}, nil),
		{ID: "B3", Desc: "Currency not CNY -> RJCT AM03 before any external call", Build: noExternalCall("B3", "currency_bad")},
		{ID: "B4", Desc: "Amount above limit -> RJCT AM02 before any external call", Build: noExternalCall("B4", "amount_over")},
		dataFault("B5", "AML HIT -> RJCT RR04, no posting", map[string]float64{"aml_hit": 100}, nil),
		dataFault("B6", "AML REVIEW cleared inside SLA -> ACCP, latency includes the review wait", map[string]float64{"review_fast": 100},
			func(r *run.Result) []string {
				return need(r.Summary.Latency.P50 >= 900, "B6 p50 %.0fms should include the ~1s review wait", r.Summary.Latency.P50)
			}),
		dataFault("B7", "AML REVIEW past SLA -> RJCT RR04 at SLA expiry, no posting", map[string]float64{"review_slow": 100},
			func(r *run.Result) []string {
				return need(r.Summary.Latency.P50 >= 4500 && r.Summary.Latency.P50 <= 6500, "B7 p50 %.0fms should be at the 5s SLA expiry", r.Summary.Latency.P50)
			}),
		dup("B8a", "Duplicate ibps.101, sequential: one workflow, one posting, one ibps.102", "dup_seq"),
		dup("B8b", "Duplicate ibps.101, concurrent (same MsgId within 5 ms)", "dup_conc"),
	}
	f = append(f, infra()...)
	return f
}

func noExternalCall(id, cat string) func(env.Variant, float64) run.Spec {
	return func(v env.Variant, rate float64) run.Spec {
		s := base(id, rate, map[string]float64{cat: 100})
		s.DurationS = 15
		s.Expect = func(r *run.Result) []string {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			e := env.New(".", v)
			validates, posts, err := cbsCalls(ctx, e)
			return join(categoriesAsExpected(r), rejectedNeverPost(r),
				need(err == nil, "cbs stats: %v", err),
				need(validates == 0 && posts == 0, "%s made external calls: %d validates, %d postings", id, validates, posts))
		}
		return s
	}
}

func infra() []Fault {
	happyOnly := func(r *run.Result) []string {
		return join(rejectedNeverPost(r), need(r.Summary.Received > 0, "no payments completed"))
	}
	return []Fault{
		{ID: "I1", Desc: "CBS validate latency +300 ms on 20% of calls -> ACCP, p99 rises, no extra rejects", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I1", rate, nil)
			s.Events = []run.Event{at(faultAtS, "cbs validate +300ms @20%", simFault("cbs", "validate", "latency", 0.2, 8000, 300))}
			s.Expect = func(r *run.Result) []string {
				return join(onlyOutcomesTolerant(r, 0.01, "ACCP"), happyOnly(r), need(r.Summary.Latency.P99 >= 300, "p99 %.0fms did not rise above the injected 300ms", r.Summary.Latency.P99))
			}
			return s
		}},
		{ID: "I2", Desc: "CBS validate HTTP 503 on 30% -> retries succeed; RJCT AB05 only where retries exhaust", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I2", rate, nil)
			s.Events = []run.Event{at(faultAtS, "cbs validate 503 @30%", simFault("cbs", "validate", "http503", 0.3, 8000, 0))}
			s.Expect = func(r *run.Result) []string {
				ab05 := float64(r.Summary.Outcomes["AB05"]) / float64(max(r.Summary.Received, 1))
				return join(onlyOutcomes(r, "ACCP", "AB05"), happyOnly(r), need(ab05 <= 0.03, "AB05 rate %.1f%% is higher than 4 consecutive failures (0.8%% of faulted calls) can explain", ab05*100))
			}
			return s
		}},
		{ID: "I3", Desc: "AML unreachable for 4 s -> in-window payments RJCT AB05; normal after; nothing stuck", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I3", rate, nil)
			s.Events = []run.Event{
				at(faultAtS, "aml reset_peer on", func(ctx context.Context, e *env.Env) error {
					return e.AddToxic(ctx, "aml", map[string]any{"name": "down", "type": "reset_peer", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"timeout": 0}})
				}),
				at(faultAtS+4, "aml reset_peer off", removeToxic("aml", "down")),
			}
			s.Expect = func(r *run.Result) []string {
				return join(onlyOutcomes(r, "ACCP", "AB05"), happyOnly(r),
					need(r.Summary.Outcomes["AB05"] > 0, "no payment was rejected during the AML outage"),
					need(r.Summary.Outcomes["ACCP"] > 0, "no payment succeeded outside the AML outage"))
			}
			return s
		}},
		{ID: "I4", Desc: "CBS commits the posting then drops the response -> status query finds POSTED; one posting; ACCP", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I4", rate, nil)
			s.Events = []run.Event{at(faultAtS, "cbs commit_then_drop @30%", simFault("cbs", "post", "commit_then_drop", 0.3, 8000, 0))}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomesTolerant(r, 0.01, "ACCP"), happyOnly(r)) }
			return s
		}},
		{ID: "I5", Desc: "CBS times out before commit -> retry with the same postingRef posts once; ACCP", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I5", rate, nil)
			s.Events = []run.Event{at(faultAtS, "cbs timeout_before_commit @30%", simFault("cbs", "post", "timeout_before_commit", 0.3, 8000, 1500))}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomesTolerant(r, 0.01, "ACCP"), happyOnly(r)) }
			return s
		}},
		{ID: "I6", Desc: "NPC callback down 8 s -> ibps.102 delivered late with the same MsgId; one logical receipt", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I6", rate, nil)
			s.Events = []run.Event{at(faultAtS, "npc callback down 8s", simFault("npc", "ibps102", "down_503", 1, 8000, 0))}
			s.Expect = func(r *run.Result) []string {
				return join(onlyOutcomesTolerant(r, 0.01, "ACCP"), happyOnly(r),
					need(r.Summary.StallCount > 0, "no receipt was delayed by the outage"),
					need(r.Summary.MultiReceipts == 0 && r.Summary.DoubleAnswered == 0, "%d payments saw more than one receipt", r.Summary.MultiReceipts))
			}
			return s
		}},
		{ID: "I7", Desc: "Kill a payment replica between steps 5 and 6, restart after 4 s -> resumes at step 6, no second posting", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I7", rate, nil)
			victim := v.Replicas[len(v.Replicas)-1] // a worker for V1a, replica 2 otherwise
			if v.Name == "v1a" {
				victim = "worker-1"
			}
			s.Events = []run.Event{
				at(3, "npc callback down 10s (holds payments between step 5 and 6)", simFault("npc", "ibps102", "down_503", 1, 10000, 0)),
				at(6, "kill "+victim, func(ctx context.Context, e *env.Env) error { return e.Kill(ctx, victim) }),
				at(10, "restart "+victim, func(ctx context.Context, e *env.Env) error { return e.Start(ctx, victim) }),
			}
			s.Expect = func(r *run.Result) []string {
				return join(onlyOutcomesTolerant(r, 0.01, "ACCP"), happyOnly(r), need(r.Summary.StallCount > 0, "no payment was stalled by the outage"))
			}
			return s
		}},
		{ID: "I8", Desc: "Kill a replica and never restart it -> every in-flight payment still reaches a terminal state", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I8", rate, nil)
			s.DurationS = 15
			victim := v.Replicas[len(v.Replicas)-1]
			if v.Name == "v1a" {
				victim = "worker-1"
			}
			s.Events = []run.Event{at(faultAtS, "kill "+victim+" (never restarted by the fault)", func(ctx context.Context, e *env.Env) error { return e.Kill(ctx, victim) })}
			if v.Engine == "dbos" {
				// DBOS Go v1.4.0 recovers a dead executor's workflows only when an executor with the same ID
				// starts again. The operator action is part of the measured result (docs/versions.md D6).
				s.DrainS = 20
				s.Events = append(s.Events, at(faultAtS+12, "operator starts a replacement executor with the same ID", func(ctx context.Context, e *env.Env) error { return e.Start(ctx, victim) }))
			}
			s.Expect = func(r *run.Result) []string {
				out := join(onlyOutcomes(r, "ACCP"), happyOnly(r))
				if v.Engine == "dbos" {
					out = append(out, need(r.Summary.StallCount > 0, "expected DBOS to leave the dead replica's payments stuck until a replacement starts, but nothing stalled")...)
				}
				return out
			}
			return s
		}},
		{ID: "I9", Desc: "Durability store restart -> in-flight payments pause then complete; none lost; stall recorded", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I9", rate, nil)
			// See docs/versions.md H7: Docker Desktop's VM does not durably persist a killed Postgres
			// container's data across restart on this laptop, even with an explicit CHECKPOINT beforehand,
			// so this cannot validate true crash durability here. Run as an observation, not a gate.
			s.Probe = true
			s.Events = []run.Event{at(faultAtS, "restart "+v.Durability, func(ctx context.Context, e *env.Env) error { return e.Restart(ctx, v.Durability) })}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomesTolerant(r, 0.01, "ACCP", "AB05"), happyOnly(r)) }
			return s
		}},
		{ID: "I10", Desc: "Temporal server (history/matching) killed -> in-flight payments complete; stall recorded (V1 only)", V1Only: true, Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I10", rate, nil)
			s.Events = []run.Event{at(faultAtS, "restart temporal server (all roles)", func(ctx context.Context, e *env.Env) error { return e.Restart(ctx, "temporal") })}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomes(r, "ACCP", "AB05"), happyOnly(r)) }
			return s
		}},
		{ID: "I11", Desc: "5 s partition between payment service and its durability store -> no duplicate postings", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I11", rate, nil)
			s.Events = []run.Event{
				at(faultAtS, "partition payment<->"+v.Durability, func(ctx context.Context, e *env.Env) error { return e.ProxyEnable(ctx, v.Proxy, false) }),
				at(faultAtS+5, "heal partition", func(ctx context.Context, e *env.Env) error { return e.ProxyEnable(ctx, v.Proxy, true) }),
			}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomes(r, "ACCP", "AB05"), happyOnly(r)) }
			return s
		}},
		{ID: "I12", Desc: "CBS ledger Postgres restart -> steps retry; exactly-once postings hold", Build: func(v env.Variant, rate float64) run.Spec {
			s := base("I12", rate, nil)
			s.Probe = true // see docs/versions.md H7: same Docker Desktop limitation as I9, applied to cbs-db.
			s.Events = []run.Event{at(faultAtS, "restart cbs-db", func(ctx context.Context, e *env.Env) error { return e.Restart(ctx, "cbs-db") })}
			s.Expect = func(r *run.Result) []string { return join(onlyOutcomesTolerant(r, 0.01, "ACCP", "AB05"), happyOnly(r)) }
			return s
		}},
	}
}

// Select returns the catalogue entries matching ids (all if empty), skipping V1-only faults for other engines.
func Select(v env.Variant, ids []string) ([]Fault, error) {
	all := Catalogue()
	if len(ids) == 0 {
		var out []Fault
		for _, f := range all {
			if f.V1Only && v.Engine != "temporal" {
				continue
			}
			out = append(out, f)
		}
		return out, nil
	}
	var out []Fault
	for _, id := range ids {
		found := false
		for _, f := range all {
			if strings.EqualFold(f.ID, id) {
				out, found = append(out, f), true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown fault %q", id)
		}
	}
	return out, nil
}
