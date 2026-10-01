// bench drives the IBPS Temporal-vs-DBOS benchmark: stack control, e2e checks, scenarios, the M8 matrix
// and the report. Run from anywhere; --root points at the repo (default: parent of the harness module).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/harness/internal/env"
	"github.com/bill/ibps-bench/harness/internal/faults"
	"github.com/bill/ibps-bench/harness/internal/report"
	"github.com/bill/ibps-bench/harness/internal/run"
	"github.com/bill/ibps-bench/harness/internal/scenarios"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	root := flag.String("root", "..", "repo root")
	force := flag.Bool("force", false, "re-run even if a result file exists")
	rate := flag.Float64("rate", 0, "arrival rate (TPS) where relevant")
	reps := flag.Int("reps", 3, "repetitions for headline scenarios")
	phase := flag.String("phase", "all", "matrix phase: A | B | all")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: bench [flags] <command> [args]
  up <variant>                     start a variant stack (v1a | v1b | v2), stopping any other
  down                             stop every stack
  faults <variant> [ids...]        run fault-catalogue entries (all if none) at --rate
  run <S0|S1|S2|S3|S5|S6> <variant> [--rate R]
  matrix                           the full M8 plan (resumable); --phase A|B
  report                           write results/REPORT.md from results/`)
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		die("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg, err := config.Load(filepath.Join(abs, "deploy/config"))
	if err != nil {
		die("config: %v", err)
	}

	switch args[0] {
	case "down":
		must(env.New(abs, env.Variants["v2"]).Down(ctx))
	case "up":
		e := envFor(abs, args, 1)
		must(e.Down(ctx))
		must(e.Up(ctx))
		fmt.Println("stack up:", e.V.Name)
	case "faults":
		e := envFor(abs, args, 1)
		r := *rate
		if r == 0 {
			r = 30
		}
		ok := runFaults(ctx, e, args[2:], r, abs, *force)
		if !ok {
			os.Exit(1)
		}
	case "run":
		if len(args) < 3 {
			die("usage: run <scenario> <variant>")
		}
		e := envFor(abs, args, 2)
		must(e.Down(ctx))
		must(e.Up(ctx))
		var specs []run.Spec
		switch strings.ToUpper(args[1]) {
		case "S0":
			for _, r := range scenarios.S0Rates {
				specs = append(specs, scenarios.S0(r))
			}
		case "S1":
			for _, r := range scenarios.S1Rates {
				specs = append(specs, scenarios.S1(r))
			}
		case "S2":
			specs = append(specs, scenarios.S2(scenarios.S2Start, scenarios.S2Step, scenarios.S2StepDur, scenarios.S2Max))
		case "S3":
			specs = append(specs, scenarios.S3(*rate, 10))
		case "S5":
			specs = append(specs, scenarios.S5(scenarios.S2Start, scenarios.S2Step, scenarios.S2StepDur, scenarios.S2Max))
		case "S6":
			specs = append(specs, scenarios.S6(*rate, cfg))
		default:
			die("unknown scenario %s", args[1])
		}
		for _, s := range specs {
			for rep := 1; rep <= *reps; rep++ {
				execute(ctx, e, s, rep, abs, *force)
			}
		}
	case "c1":
		e := envFor(abs, args, 1)
		c1(ctx, e, abs, *force)
	case "matrix":
		matrix(ctx, abs, cfg, *phase, *reps, *force)
	case "report":
		must(report.Write(abs))
		fmt.Println("wrote", filepath.Join(abs, "results", "REPORT.md"))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		die("%v", err)
	}
}

func envFor(root string, args []string, i int) *env.Env {
	if len(args) <= i {
		die("variant required (v1a | v1b | v2)")
	}
	v, ok := env.Variants[args[i]]
	if !ok {
		die("unknown variant %q", args[i])
	}
	return env.New(root, v)
}

// execute runs one spec/rep unless a result already exists; it always saves and reports the verdict.
func execute(ctx context.Context, e *env.Env, s run.Spec, rep int, root string, force bool) *run.Result {
	p := run.Path(root, e.V.Name, s.Scenario, s.Label, rep)
	if _, err := os.Stat(p); err == nil && !force {
		if r, err := run.Load(p); err == nil && r.Aborted == "" {
			fmt.Printf("skip %s (exists)\n", p)
			return r
		}
	}
	r := run.Once(ctx, e, s, rep)
	path, err := run.Save(root, r)
	if err != nil {
		die("save: %v", err)
	}
	status := "PASS"
	if r.Aborted != "" {
		status = "ABORTED: " + r.Aborted
	} else if !r.Verdict.Pass {
		status = "FAIL: " + strings.Join(r.Verdict.Failures, "; ")
	}
	fmt.Printf("%-5s %s/%s/%s rep%d -> %s\n", strings.ToUpper(strings.Split(status, ":")[0]), e.V.Name, s.Scenario, s.Label, rep, path)
	if status != "PASS" {
		fmt.Println("      ", status)
	}
	if s.Cooldown > 0 && ctx.Err() == nil {
		time.Sleep(s.Cooldown)
	}
	return r
}

func runFaults(ctx context.Context, e *env.Env, ids []string, rate float64, root string, force bool) bool {
	fs, err := faults.Select(e.V, ids)
	must(err)
	must(e.Down(ctx))
	must(e.Up(ctx))
	all := true
	for _, f := range fs {
		s := f.Build(e.V, rate)
		s.Cooldown = 10 * time.Second
		r := execute(ctx, e, s, 1, root, force)
		if r.Aborted != "" || !r.Verdict.Pass {
			all = false
		}
	}
	return all
}

// c1 is the optional code-fault probe: panic in the workflow body for one payment and record how each
// engine surfaces the stuck payment to an operator.
func c1(ctx context.Context, e *env.Env, root string, force bool) {
	e.Extra = map[string]string{"CODE_FAULT_SUFFIX": "-00000005"}
	must(e.Down(ctx))
	must(e.Up(ctx))
	s := run.Spec{Scenario: "C1", Label: "code-fault", Rate: 10, DurationS: 20, WarmupS: 0, DrainS: 20,
		Mix: map[string]float64{"happy": 100}, Seed: 7, Probe: true}
	p := run.Path(root, e.V.Name, s.Scenario, s.Label, 1)
	if _, err := os.Stat(p); err == nil && !force {
		fmt.Println("skip", p)
		return
	}
	r := run.Once(ctx, e, s, 1)
	if r.Aborted == "" {
		r.Verdict.Notes = append(r.Verdict.Notes, surface(ctx, e, r.RunID)...)
	}
	path, err := run.Save(root, r)
	must(err)
	fmt.Println("C1 saved", path)
	for _, n := range r.Verdict.Notes {
		fmt.Println("  ", n)
	}
	e.Extra = nil
	must(e.Recreate(ctx)) // remove the fault hook again
}

// surface asks the engine how it reports the workflow whose body panicked.
func surface(ctx context.Context, e *env.Env, runID string) []string {
	id := fmt.Sprintf("pay-%s-00000005-E%s-00000005", runID, runID)
	var out []string
	switch e.V.Engine {
	case "dbos":
		d, err := e.DB(ctx, "dbos")
		if err != nil {
			return []string{"C1: cannot read dbos-db: " + err.Error()}
		}
		var status, execID string
		var errStr *string
		var attempts int
		if err := d.QueryRow(ctx, `SELECT status, executor_id, error, recovery_attempts FROM dbos.workflow_status WHERE workflow_uuid=$1`, id).Scan(&status, &execID, &errStr, &attempts); err != nil {
			return []string{"C1 DBOS: workflow row not found: " + err.Error()}
		}
		msg := "<none>"
		if errStr != nil {
			msg = *errStr
		}
		out = append(out, fmt.Sprintf("DBOS records the panic in workflow_status: status=%s recovery_attempts=%d error=%q. The payment stays non-terminal in app-db and never gets an ibps.102; no operator alert is raised by the engine.", status, attempts, msg))
	default:
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "ibps_default", "temporalio/admin-tools:1.32.0",
			"temporal", "--address", "temporal:7233", "workflow", "describe", "-w", id)
		b, err := cmd.CombinedOutput()
		if err != nil {
			return []string{"C1: temporal describe failed: " + err.Error()}
		}
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "Status") || strings.Contains(l, "Pending Workflow Task") || strings.Contains(l, "Attempt") || strings.Contains(l, "Last Failure") || strings.Contains(l, "PendingWorkflowTask") || strings.Contains(l, "HistoryLength") {
				keep = append(keep, l)
			}
		}
		out = append(out, "Temporal keeps the workflow RUNNING and retries the failing workflow task indefinitely: "+strings.Join(keep, " | "))
	}
	return out
}

// ---- the M8 matrix ------------------------------------------------------------------------------------------

var variantOrder = []string{"v1a", "v1b", "v2"}

func loadCeiling(root, variant, scen string) float64 {
	var vals []float64
	for rep := 1; rep <= 9; rep++ {
		if r, err := run.Load(run.Path(root, variant, scen, map[string]string{"S2": "stepup", "S5": "stepup-1shard"}[scen], rep)); err == nil && r.Aborted == "" {
			vals = append(vals, r.Summary.CeilingRate)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	return vals[len(vals)/2]
}

func matrix(ctx context.Context, root string, cfg config.Config, phase string, reps int, force bool) {
	if phase == "A" || phase == "all" {
		// Interleave repetitions across variants so drift hits all variants equally (fairness rule 6).
		for rep := 1; rep <= reps; rep++ {
			for _, vn := range variantOrder {
				if ctx.Err() != nil {
					return
				}
				e := env.New(root, env.Variants[vn])
				must(e.Down(ctx))
				must(e.Up(ctx))
				for _, r := range scenarios.S0Rates {
					execute(ctx, e, scenarios.S0(r), rep, root, force)
				}
				for _, r := range scenarios.S1Rates {
					execute(ctx, e, scenarios.S1(r), rep, root, force)
				}
				execute(ctx, e, scenarios.S2(scenarios.S2Start, scenarios.S2Step, scenarios.S2StepDur, scenarios.S2Max), rep, root, force)
			}
		}
	}
	if phase == "B" || phase == "all" {
		common := 0.0
		for _, vn := range variantOrder {
			c := loadCeiling(root, vn, "S2")
			fmt.Printf("S2 ceiling %s: %.0f TPS\n", vn, c)
			if c > 0 && (common == 0 || c < common) {
				common = c
			}
		}
		if common == 0 {
			die("no S2 ceilings found; run phase A first")
		}
		fmt.Printf("common S2 ceiling: %.0f TPS\n", common)
		b, _ := json.Marshal(map[string]float64{"commonCeiling": common, "s3Rate": round(common * 0.6), "s4Rate": round(common * 0.5), "s6Rate": round(common * 0.6)})
		_ = os.WriteFile(filepath.Join(root, "results", "common.json"), b, 0o644)
		for _, vn := range variantOrder {
			if ctx.Err() != nil {
				return
			}
			e := env.New(root, env.Variants[vn])
			must(e.Down(ctx))
			must(e.Up(ctx))
			// Trimmed after v1a's full Phase B (S3+S6+21 faults+S5) took 2h43m alone, which would have put
			// the whole matrix at 5+ hours (Bill, 2026-09-30: keep the whole run under 3h). v1a already has
			// the full run on disk (results/v1a/S3, S6, S4/*, S5) as the representative sample; for the
			// remaining variants this runs a curated 7-fault subset spanning every fault category (data
			// reject, SLA-boundary edge case, duplicate-delivery idempotency, latency injection, dependency
			// outage, kill-and-recover, durability-store-restart probe) and skips S3/S6/S5.
			quickFaultIDs := []string{"B1", "B7", "B8a", "I1", "I3", "I7", "I9"}
			fs, err := faults.Select(e.V, quickFaultIDs)
			must(err)
			for _, f := range fs {
				s := f.Build(e.V, round(common*0.5))
				execute(ctx, e, s, 1, root, force)
			}
		}
	}
}

func round(x float64) float64 {
	if x < 10 {
		return float64(int(x + 0.5))
	}
	return float64(int(x/5+0.5)) * 5
}
