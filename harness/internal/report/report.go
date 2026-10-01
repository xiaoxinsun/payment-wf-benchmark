// Package report renders results/REPORT.md (and SVG charts) from the stored run results.
package report

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bill/ibps-bench/harness/internal/faults"
	"github.com/bill/ibps-bench/harness/internal/run"
)

var variants = []string{"v1a", "v1b", "v2"}

type data map[string]map[string]map[string][]*run.Result // variant -> scenario -> label -> reps

func load(root string) (data, int, error) {
	d := data{}
	n := 0
	err := filepath.WalkDir(filepath.Join(root, "results"), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".json") || filepath.Base(p) == "common.json" {
			return nil
		}
		r, err := run.Load(p)
		if err != nil || r.Variant == "" {
			return nil
		}
		if d[r.Variant] == nil {
			d[r.Variant] = map[string]map[string][]*run.Result{}
		}
		if d[r.Variant][r.Spec.Scenario] == nil {
			d[r.Variant][r.Spec.Scenario] = map[string][]*run.Result{}
		}
		d[r.Variant][r.Spec.Scenario][r.Spec.Label] = append(d[r.Variant][r.Spec.Scenario][r.Spec.Label], r)
		n++
		return nil
	})
	for _, sc := range d {
		for _, lb := range sc {
			for _, reps := range lb {
				sort.Slice(reps, func(i, j int) bool { return reps[i].Rep < reps[j].Rep })
			}
		}
	}
	return d, n, err
}

// median returns the median run by p99 (the plan's "median run and the spread").
func median(reps []*run.Result) *run.Result {
	var ok []*run.Result
	for _, r := range reps {
		if r.Aborted == "" {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].Summary.Latency.P99 < ok[j].Summary.Latency.P99 })
	return ok[len(ok)/2]
}

func spread(reps []*run.Result, f func(*run.Result) float64) (lo, hi float64) {
	first := true
	for _, r := range reps {
		if r.Aborted != "" {
			continue
		}
		v := f(r)
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	return
}

func (d data) get(v, sc, label string) []*run.Result { return d[v][sc][label] }

func labels(d data, sc string) []string {
	set := map[string]bool{}
	for _, v := range variants {
		for l := range d[v][sc] {
			set[l] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

func ms(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.2f s", v/1000)
	case v >= 10:
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%.1f ms", v)
}

func verdictCell(r *run.Result) string {
	switch {
	case r == nil:
		return "not run"
	case r.Aborted != "":
		return "ABORTED"
	case r.Verdict.Pass:
		return "PASS"
	}
	return "FAIL"
}

// Write generates results/REPORT.md and results/charts/*.svg.
func Write(root string) error {
	d, n, err := load(root)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no results under %s/results", root)
	}
	chartDir := filepath.Join(root, "results", "charts")
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	saveChart := func(name, svg string) string {
		_ = os.WriteFile(filepath.Join(chartDir, name+".svg"), []byte(svg), 0o644)
		return fmt.Sprintf("![%s](charts/%s.svg)", name, name)
	}

	w("# IBPS inward payment benchmark: Temporal vs DBOS results")
	w("")
	w("Generated %s from %d run files. Laptop-scale experiment (Apple M3 Pro, 12 cores, 36 GB, Docker Desktop VM); read the **relative** numbers, not the absolute ones. Versions and the M0 gate are in `docs/versions.md`.", time.Now().Format("2006-01-02 15:04"), n)
	w("")
	w("Variants: **V1a** Temporal baseline (regular activities, separate ingress and worker deployments), **V1b** Temporal optimised (co-located, eager start, Local Activities for steps 3, 4 and state writes), **V2** DBOS (in-process, steps checkpointed straight to Postgres).")
	w("")

	correctness(&b, d)
	latencySection(&b, d, "S0", "S0 engine overhead (simulators at 0 ms latency)", "Isolates what the engine itself costs per payment: with no external latency, the numbers below are engine scheduling, checkpointing and database round trips.", saveChart)
	latencySection(&b, d, "S1", "S1 latency at fixed arrival rates (simulators at 5/10/3 ms)", "End-to-end latency from ibps.101 scheduled to ibps.102 received, open-model arrivals, 60 s warm-up excluded, median of 3 runs (spread = min to max of the three runs' p99).", saveChart)
	costSection(&b, d, saveChart)
	s2Section(&b, d, saveChart)
	s3Section(&b, d, saveChart)
	s4Section(&b, d)
	s5Section(&b, d)
	s6Section(&b, d)
	footprintSection(&b, d)
	operationalSection(&b, d)
	caveats(&b)

	return os.WriteFile(filepath.Join(root, "results", "REPORT.md"), []byte(b.String()), 0o644)
}

// ---- sections ------------------------------------------------------------------------------------------------

func correctness(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	w("## Correctness (pass criteria)")
	w("")
	w("A variant that fails correctness is reported as failed regardless of latency. Checks after every run: ledger legs sum to zero; postings = ACCEPTED_SENT payments; no posting for a rejected payment; no duplicate posting reference; no payment answered by two different ibps.102 messages; nothing acknowledged is left non-terminal or lost 60 s after load stops.")
	w("")
	w("| Variant | Runs | Passed | Failed | Aborted |")
	w("| --- | --- | --- | --- | --- |")
	type fail struct{ where, msg string }
	var fails []fail
	for _, v := range variants {
		runs, pass, failed, aborted := 0, 0, 0, 0
		for sc, lbs := range d[v] {
			for lb, reps := range lbs {
				for _, r := range reps {
					runs++
					switch {
					case r.Aborted != "":
						aborted++
						fails = append(fails, fail{fmt.Sprintf("%s %s/%s rep%d", v, sc, lb, r.Rep), "aborted: " + r.Aborted})
					case r.Verdict.Pass:
						pass++
					default:
						failed++
						fails = append(fails, fail{fmt.Sprintf("%s %s/%s rep%d", v, sc, lb, r.Rep), strings.Join(r.Verdict.Failures, "; ")})
					}
				}
			}
		}
		w("| %s | %d | %d | %d | %d |", variantName[v], runs, pass, failed, aborted)
	}
	w("")
	if len(fails) == 0 {
		w("Every run of every variant satisfied every invariant.")
	} else {
		sort.Slice(fails, func(i, j int) bool { return fails[i].where < fails[j].where })
		w("Runs that did not pass:")
		w("")
		for _, f := range fails {
			w("- `%s`: %s", f.where, f.msg)
		}
	}
	w("")
}

func latencySection(b *strings.Builder, d data, sc, title, intro string, saveChart func(string, string) string) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	lbs := labels(d, sc)
	if len(lbs) == 0 {
		return
	}
	w("## %s", title)
	w("")
	w("%s", intro)
	w("")
	w("| Rate | Variant | p50 | p95 | p99 | p99.9 | max | p99 spread (3 runs) | Achieved TPS | SLA breach |")
	w("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	var cats []string
	ser := map[string]*series{}
	for _, v := range variants {
		ser[v] = &series{Name: variantName[v], Colour: variantColour[v]}
	}
	for _, lb := range lbs {
		cats = append(cats, strings.TrimPrefix(lb, "rate")+" TPS")
		for _, v := range variants {
			reps := d.get(v, sc, lb)
			m := median(reps)
			if m == nil {
				w("| %s | %s | not run | | | | | | | |", strings.TrimPrefix(lb, "rate"), variantName[v])
				ser[v].Vals = append(ser[v].Vals, 0)
				continue
			}
			lo, hi := spread(reps, func(r *run.Result) float64 { return r.Summary.Latency.P99 })
			l := m.Summary.Latency
			w("| %s TPS | %s | %s | %s | %s | %s | %s | %s to %s | %.1f | %.2f%% |", strings.TrimPrefix(lb, "rate"), variantName[v],
				ms(l.P50), ms(l.P95), ms(l.P99), ms(l.P999), ms(l.Max), ms(lo), ms(hi), m.Summary.AchievedTPS, m.Summary.SLABreachRate*100)
			ser[v].Vals = append(ser[v].Vals, l.P99)
		}
	}
	w("")
	var ss []series
	for _, v := range variants {
		ss = append(ss, *ser[v])
	}
	w("%s", saveChart(strings.ToLower(sc)+"-p99", barChart(sc+" p99 latency by arrival rate (log scale)", "p99, ms", cats, ss, true)))
	w("")
	// p50 chart too
	ser50 := map[string]*series{}
	for _, v := range variants {
		ser50[v] = &series{Name: variantName[v], Colour: variantColour[v]}
		for _, lb := range lbs {
			if m := median(d.get(v, sc, lb)); m != nil {
				ser50[v].Vals = append(ser50[v].Vals, m.Summary.Latency.P50)
			} else {
				ser50[v].Vals = append(ser50[v].Vals, 0)
			}
		}
	}
	var s50 []series
	for _, v := range variants {
		s50 = append(s50, *ser50[v])
	}
	w("%s", saveChart(strings.ToLower(sc)+"-p50", barChart(sc+" median latency by arrival rate", "p50, ms", cats, s50, false)))
	w("")
}

func costSection(b *strings.Builder, d data, saveChart func(string, string) string) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	lbs := labels(d, "S1")
	if len(lbs) == 0 {
		return
	}
	// Highest S1 rate every variant ran, so the durability counters reflect real load, not an idle run.
	label := lbs[len(lbs)-1]
	w("## Durability cost per payment")
	w("")
	w("The most transferable result: how much the durability database is asked to do for one payment, measured from Postgres counters (`pg_stat_database`, `pg_stat_wal`, `pg_stat_statements`) over a whole run and divided by completed payments. It does not depend on the laptop's speed. Happy-path payments, S1 at %s TPS, median run.", strings.TrimPrefix(label, "rate"))
	w("")
	w("| Variant | Transactions | Rows written | Statements | WAL bytes |")
	w("| --- | --- | --- | --- | --- |")
	keys := []string{"durabilityXacts", "durabilityRowsWritten", "durabilityStatements", "durabilityWalBytes"}
	vals := map[string][]float64{}
	any := false
	for _, v := range variants {
		m := median(d.get(v, "S1", label))
		if m == nil {
			w("| %s | not run | | | |", variantName[v])
			vals[v] = []float64{0, 0, 0, 0}
			continue
		}
		any = true
		row := []float64{}
		for _, k := range keys {
			row = append(row, m.PerPay[k])
		}
		vals[v] = row
		w("| %s | %.0f | %.0f | %.0f | %.0f |", variantName[v], row[0], row[1], row[2], row[3])
	}
	w("")
	if any {
		cats := []string{"transactions", "rows written", "statements"}
		var ss []series
		for _, v := range variants {
			ss = append(ss, series{Name: variantName[v], Colour: variantColour[v], Vals: vals[v][:3]})
		}
		w("%s", saveChart("cost-per-payment", barChart("Durability database work per payment", "count per payment", cats, ss, false)))
		w("")
	}
}

func s2Section(b *strings.Builder, d data, saveChart func(string, string) string) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	if len(d["v1a"]["S2"])+len(d["v1b"]["S2"])+len(d["v2"]["S2"]) == 0 {
		return
	}
	w("## S2 maximum sustainable throughput")
	w("")
	w("Arrival rate stepped up (50 TPS steps of 60 s, happy path only) until a step's p99 exceeded the SLA (5 s) or its error rate exceeded 0.1%%. The ceiling is the last step that stayed inside both. Errors are ingress failures plus payments unanswered after 2x the SLA.")
	w("")
	w("| Variant | Ceiling per run | Median ceiling | Breach reason (median run) |")
	w("| --- | --- | --- | --- |")
	var ss []series
	for _, v := range variants {
		reps := d.get(v, "S2", "stepup")
		m := median(reps)
		if m == nil {
			w("| %s | not run | | |", variantName[v])
			continue
		}
		var per []string
		var ceil []float64
		for _, r := range reps {
			if r.Aborted == "" {
				per = append(per, fmt.Sprintf("%.0f", r.Summary.CeilingRate))
				ceil = append(ceil, r.Summary.CeilingRate)
			}
		}
		sort.Float64s(ceil)
		reason := "not reached (rate cap)"
		for _, st := range m.Summary.Steps {
			if st.Breach {
				reason = fmt.Sprintf("%.0f TPS: %s (p99 %s, errors %.2f%%)", st.Rate, st.Reason, ms(st.P99Ms), st.ErrorRate*100)
				break
			}
		}
		w("| %s | %s TPS | %.0f TPS | %s |", variantName[v], strings.Join(per, ", "), ceil[len(ceil)/2], reason)
		s := series{Name: variantName[v], Colour: variantColour[v]}
		for _, st := range m.Summary.Steps {
			s.Xs = append(s.Xs, st.Rate)
			s.Vals = append(s.Vals, st.P99Ms)
		}
		ss = append(ss, s)
	}
	w("")
	if len(ss) > 0 {
		w("%s", saveChart("s2-stepup", lineChart("S2 p99 per step (median run)", "arrival rate, TPS", "p99, ms", ss, 5000, "SLA 5 s")))
		w("")
	}
}

func s3Section(b *strings.Builder, d data, saveChart func(string, string) string) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	lbs := labels(d, "S3")
	if len(lbs) == 0 {
		return
	}
	w("## S3 soak")
	w("")
	w("10 minutes at 60%% of the common S2 ceiling (shortened from 60 minutes for the laptop). Stability means p99 in the last third of the run is no worse than in the first third; the memory and database columns show growth.")
	w("")
	w("| Variant | Rate | p50 | p99 (first third) | p99 (last third) | Max memory: payment | Durability DB rows written | Pass |")
	w("| --- | --- | --- | --- | --- | --- | --- | --- |")
	var ss []series
	for _, v := range variants {
		for _, lb := range lbs {
			m := median(d.get(v, "S3", lb))
			if m == nil {
				w("| %s | | not run | | | | | |", variantName[v])
				continue
			}
			tl := m.Summary.Timeline
			third := len(tl) / 3
			p99 := func(a, z int) float64 {
				best := 0.0
				for _, s := range tl[a:z] {
					if s.P99 > best {
						best = s.P99
					}
				}
				return best
			}
			first, last := 0.0, 0.0
			if third > 0 {
				first, last = p99(0, third), p99(len(tl)-third, len(tl))
			}
			mem := 0.0
			for name, rs := range m.Resources {
				if strings.HasPrefix(name, "payment") || strings.HasPrefix(name, "worker") {
					if rs.MaxMemMiB > mem {
						mem = rs.MaxMemMiB
					}
				}
			}
			dur := m.DB[map[string]string{"v2": "dbos"}[v]]
			if v != "v2" {
				dur = m.DB["temporal"]
			}
			w("| %s | %s TPS | %s | %s | %s | %.0f MiB | %d | %s |", variantName[v], strings.TrimPrefix(lb, "rate"), ms(m.Summary.Latency.P50), ms(first), ms(last), mem, dur.TupIns+dur.TupUpd+dur.TupDel, verdictCell(m))
			s := series{Name: variantName[v], Colour: variantColour[v]}
			for _, sec := range tl {
				if sec.Recv > 0 {
					s.Xs = append(s.Xs, float64(sec.T))
					s.Vals = append(s.Vals, sec.P99)
				}
			}
			ss = append(ss, s)
		}
	}
	w("")
	if len(ss) > 0 {
		w("%s", saveChart("s3-soak", lineChart("S3 soak: p99 per second", "seconds after warm-up", "p99, ms", ss, 0, "")))
		w("")
	}
}

func s4Section(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	total := 0
	for _, v := range variants {
		total += len(d[v]["S4"])
	}
	if total == 0 {
		return
	}
	w("## S4 fault matrix")
	w("")
	w("One fault per run at 50%% of the common S2 ceiling (or the rate shown in the run file for the acceptance runs). Every fault has one asserted expected outcome. *Recovery* is the time from the last disruptive event until the last stalled payment (slower than 2 s) was answered; *stall* is the span from the first stalled payment being sent to the last being answered.")
	w("")
	w("| Fault | Description | V1a | V1b | V2 |")
	w("| --- | --- | --- | --- | --- |")
	cat := faults.Catalogue()
	for _, f := range cat {
		row := []string{}
		for _, v := range variants {
			r := median(d.get(v, "S4", f.ID))
			if f.V1Only && v == "v2" {
				row = append(row, "n/a (V1 only)")
				continue
			}
			c := verdictCell(r)
			if r != nil && r.Aborted == "" {
				extra := ""
				if r.Summary.StallCount > 0 {
					extra = fmt.Sprintf(" stall %.1fs", r.Summary.StallSpanMs/1000)
				}
				if r.RecoveryS > 0 {
					extra += fmt.Sprintf(", recovery %.1fs", r.RecoveryS)
				}
				c += extra
			}
			row = append(row, c)
		}
		w("| %s | %s | %s |", f.ID, f.Desc, strings.Join(row, " | "))
	}
	w("")
	w("Latency impact (p99 during the whole run) and outcomes:")
	w("")
	w("| Fault | Variant | p50 | p99 | max | Outcomes | Unanswered | Ingress errors |")
	w("| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, f := range cat {
		for _, v := range variants {
			r := median(d.get(v, "S4", f.ID))
			if r == nil {
				continue
			}
			var oc []string
			for k, n := range r.Summary.Outcomes {
				oc = append(oc, fmt.Sprintf("%s %d", k, n))
			}
			sort.Strings(oc)
			w("| %s | %s | %s | %s | %s | %s | %d | %d |", f.ID, variantName[v], ms(r.Summary.Latency.P50), ms(r.Summary.Latency.P99), ms(r.Summary.Latency.Max), strings.Join(oc, ", "), r.Summary.Unanswered, r.Summary.IngressErr)
		}
	}
	w("")
	for _, v := range variants {
		for _, f := range cat {
			if r := median(d.get(v, "S4", f.ID)); r != nil && r.Aborted == "" && !r.Verdict.Pass {
				w("- **%s %s failed:** %s", variantName[v], f.ID, strings.Join(r.Verdict.Failures, "; "))
			}
		}
	}
	w("")
}

func s5Section(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	any := false
	for _, v := range variants {
		if len(d[v]["S5"]) > 0 {
			any = true
		}
	}
	if !any {
		return
	}
	w("## S5 hot settlement account")
	w("")
	w("S2 repeated with a single settlement account (N = 1) against the default 16 sub-accounts: the share of the ceiling that is the ledger, not the engine.")
	w("")
	w("| Variant | Ceiling, 16 shards | Ceiling, 1 shard | Change |")
	w("| --- | --- | --- | --- |")
	for _, v := range variants {
		a, s := median(d.get(v, "S2", "stepup")), median(d.get(v, "S5", "stepup-1shard"))
		if a == nil || s == nil {
			w("| %s | not run | | |", variantName[v])
			continue
		}
		ch := "n/a"
		if a.Summary.CeilingRate > 0 {
			ch = fmt.Sprintf("%+.0f%%", (s.Summary.CeilingRate/a.Summary.CeilingRate-1)*100)
		}
		w("| %s | %.0f TPS | %.0f TPS | %s |", variantName[v], a.Summary.CeilingRate, s.Summary.CeilingRate, ch)
	}
	w("")
}

func s6Section(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	lbs := labels(d, "S6")
	if len(lbs) == 0 {
		return
	}
	w("## S6 business mix")
	w("")
	w("Default mix (94%% happy path, 3%% account rejects, 1%% AML hit, 1%% REVIEW-FAST, 1%% REVIEW-SLOW). p50 by category; REVIEW-SLOW payments are rejected at the 5 s SLA by design.")
	w("")
	w("| Category | %s | %s | %s |", variantName["v1a"], variantName["v1b"], variantName["v2"])
	w("| --- | --- | --- | --- |")
	cats := map[string]bool{}
	for _, v := range variants {
		if m := median(d.get(v, "S6", lbs[0])); m != nil {
			for c := range m.Summary.ByCategory {
				cats[c] = true
			}
		}
	}
	var names []string
	for c := range cats {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		row := []string{}
		for _, v := range variants {
			m := median(d.get(v, "S6", lbs[0]))
			if m == nil || m.Summary.ByCategory[c] == nil {
				row = append(row, "n/a")
				continue
			}
			cs := m.Summary.ByCategory[c]
			row = append(row, fmt.Sprintf("%s (n=%d, %d unexpected)", ms(cs.Latency.P50), cs.Sent, cs.Mismatch))
		}
		w("| %s | %s |", c, strings.Join(row, " | "))
	}
	w("")
}

func footprintSection(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	rows := 0
	for _, v := range variants {
		if median(d.get(v, "S1", "rate100")) != nil {
			rows++
		}
	}
	if rows == 0 {
		return
	}
	w("## Resource footprint")
	w("")
	w("Mean CPU cores used (docker stats, whole run) at 100 TPS, S1 median run. The Temporal server and its database are part of V1's footprint; V2 needs only its Postgres.")
	w("")
	w("| Component | %s | %s | %s |", variantName["v1a"], variantName["v1b"], variantName["v2"])
	w("| --- | --- | --- | --- |")
	groups := []struct {
		name string
		f    func(string) bool
	}{
		{"Payment service (ingress, workers)", func(n string) bool {
			return strings.HasPrefix(n, "payment") || strings.HasPrefix(n, "ingress") || strings.HasPrefix(n, "worker")
		}},
		{"Temporal server", func(n string) bool { return n == "temporal" }},
		{"Durability Postgres (temporal-db / dbos-db)", func(n string) bool { return n == "temporal-db" || n == "dbos-db" }},
		{"app-db + cbs-db", func(n string) bool { return n == "app-db" || n == "cbs-db" }},
		{"Simulators + LB + toxiproxy", func(n string) bool {
			return strings.HasPrefix(n, "sim-") || n == "lb" || n == "toxiproxy"
		}},
	}
	tot := map[string]float64{}
	for _, g := range groups {
		row := []string{}
		for _, v := range variants {
			m := median(d.get(v, "S1", "rate100"))
			if m == nil {
				row = append(row, "n/a")
				continue
			}
			s := 0.0
			for n, rs := range m.Resources {
				if g.f(n) {
					s += rs.MeanCores
				}
			}
			tot[v] += s
			row = append(row, fmt.Sprintf("%.2f", s))
		}
		w("| %s | %s |", g.name, strings.Join(row, " | "))
	}
	w("| **Total** | **%.2f** | **%.2f** | **%.2f** |", tot["v1a"], tot["v1b"], tot["v2"])
	w("")
}

func operationalSection(b *strings.Builder, d data) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	w("## Operational differences observed")
	w("")
	w("- **Dead replica (I8).** Temporal reassigns an orphaned workflow to any live worker automatically. DBOS Go v1.4.0 recovers a workflow only when an executor with the same ID starts again: a live replica with a different ID left the orphan PENDING (kill test in `docs/versions.md` D6). Cross-executor recovery needs DBOS Conductor (external service) or the deprecated admin server. In the I8 run for V2 the payments owned by the killed replica stayed stuck until the harness, acting as the operator, restarted an executor with the same ID.")
	w("- **Retry timing.** Temporal regular-activity retries have a floor of about 1 s on the server's timer queue (configured 50 ms x2 backoff gave 1.0 s gaps); Local Activities and DBOS steps retry on the configured schedule (`docs/versions.md` G6).")
	w("- **Update-with-Start plus eager start** cannot be combined in Temporal Go SDK v1.49.0 (silently ignored), so V1b uses eager start only (`docs/versions.md` G4).")
	w("- **Per-step timeout.** DBOS Go has no per-step timeout option; per-attempt timeouts come from the HTTP client and context deadline inside the shared core, identically for every engine.")
	for _, v := range variants {
		if rs := d.get(v, "C1", "code-fault"); len(rs) > 0 && rs[0].Aborted == "" {
			w("- **Code fault probe C1, %s:** %s", variantName[v], strings.Join(rs[0].Verdict.Notes, " "))
		}
	}
	w("")
}

func caveats(b *strings.Builder) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	w("## Caveats and deviations from the plan")
	w("")
	w("- **Laptop, one machine, everything containerised in Docker Desktop's VM.** Load generator, simulators, engines and databases compete for the same 12 cores; results are relative. Docker Desktop does not give real fsync-to-disk semantics, so `synchronous_commit = on` costs less than on a production server, for both engines equally.")
	w("- **Scaled down further for a same-day, indicative run (2026-09-30):** whole matrix targeted at under 3 hours rather than the plan's 6-8. Rates 30/60 TPS for S1 (was 25-100), S2 steps of 30 TPS every 15s (was 50/60s), a 3-minute soak (was 10), 15-20s warm-ups (was 60s), and **1 repetition instead of 3** for S0-S2 — so the p99 spread columns below have no spread to report. Every fault's injected-fault duration and drain window is compressed by roughly 2.5x (documented in `internal/faults/faults.go`). Read this as a relative comparison, not a statistically polished result.")
	w("- **CPU budget:** payment service 2 CPUs in total for every variant (V1a splits it 0.3 ingress + 0.7 worker per pair); Temporal server 3 CPUs; both durability Postgres instances 3 CPUs (identical spec). The Temporal server's CPU is counted in V1's footprint.")
	w("- **Observability** is Prometheus metrics, pprof and `docker stats`, not OpenTelemetry spans; engine overhead is read from S0 and from the durability cost counters.")
	w("- **I10** restarts the whole Temporal server container (frontend, history, matching and worker roles are co-hosted), not one role.")
	w("- **DBOS ingress** runs workflows directly in-process with an in-flight semaphore per replica instead of a DBOS queue, following the plan's wording; queue concurrency limits were verified in the M0 gate.")
	w("- **I9 and I12 (durability-store Postgres restart) run as observational probes, not gated pass/fail checks.** On this laptop's Docker Desktop, a database created by `CREATE DATABASE` after a Postgres container's first boot does not reliably survive `docker restart` of that container, independent of disk-backed vs RAM-backed volumes and even with an explicit `CHECKPOINT` immediately before the kill (`docs/versions.md` H7). This is a Docker Desktop storage property on this machine, affects both engines equally, and means true crash-durability across a database-container restart cannot be validated here. I7 (kill/restart a stateless payment-service replica) and I10 (restart the Temporal server, not its Postgres) are unaffected and remain hard-gated.")
	w("- The IBPS message schema, SLA (5 s), amount limit and reject-code table are placeholders (see the hand-off document).")
	w("")
}
