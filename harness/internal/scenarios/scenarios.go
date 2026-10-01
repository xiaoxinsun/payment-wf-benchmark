// Package scenarios defines S0-S6 and the M8 matrix (what runs, in which order, at which rates).
//
// Sized for an indicative run under 3 hours on the laptop (Bill, 2026-09-30), not the fully-fledged
// timings in the hand-off doc: shorter warm-ups, fewer rate points, a tighter S2 step-up and 1 repetition
// instead of 3. This trades statistical polish for a same-day relative comparison; see the report's
// caveats section.
package scenarios

import (
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/harness/internal/run"
	"github.com/bill/ibps-bench/sims/npc"
)

const (
	Warmup   = 20 // seconds, every headline scenario
	Measured = 45
	cooldown = 10 * time.Second
)

func happy() map[string]float64 { return map[string]float64{"happy": 100} }

// zeroSim removes external latency so only engine cost remains (S0).
var zeroSim = map[string]float64{"cbs": 0, "aml": 0, "npc": 0}

func S0(rate float64) run.Spec {
	return run.Spec{Scenario: "S0", Label: label("rate", rate), Rate: rate, DurationS: Measured, WarmupS: 15, DrainS: 15,
		Mix: happy(), Seed: 1, SimMs: zeroSim, Cooldown: cooldown}
}

func S1(rate float64) run.Spec {
	return run.Spec{Scenario: "S1", Label: label("rate", rate), Rate: rate, DurationS: Measured, WarmupS: Warmup, DrainS: 15,
		Mix: happy(), Seed: 1, Cooldown: cooldown}
}

// S2 steps the arrival rate up until p99 breaches the SLA or errors exceed 0.1%.
func S2(start, step float64, stepDurS int, max float64) run.Spec {
	return run.Spec{Scenario: "S2", Label: "stepup", DurationS: 1, DrainS: 15, Mix: happy(), Seed: 1,
		StepUp:   &npc.StepUp{StartRate: start, Step: step, StepDurS: stepDurS, MaxRate: max},
		Cooldown: 15 * time.Second}
}

func S3(rate float64, minutes int) run.Spec {
	return run.Spec{Scenario: "S3", Label: label("rate", rate), Rate: rate, DurationS: minutes * 60, WarmupS: Warmup, DrainS: 15,
		Mix: happy(), Seed: 1, Cooldown: cooldown}
}

// S5 repeats the step-up with one settlement account (hot row).
func S5(start, step float64, stepDurS int, max float64) run.Spec {
	s := S2(start, step, stepDurS, max)
	s.Scenario, s.Label, s.Shards = "S5", "stepup-1shard", 1
	return s
}

// S6 runs the default business mix from mix.yml.
func S6(rate float64, cfg config.Config) run.Spec {
	m := cfg.Mix
	return run.Spec{Scenario: "S6", Label: label("rate", rate), Rate: rate, DurationS: 60, WarmupS: Warmup, DrainS: 20,
		Mix:  npc.DefaultMix(float64(m.HappyPath), float64(m.AccountReject), float64(m.AMLHit), float64(m.ReviewFast), float64(m.ReviewSlow)),
		Seed: 1, Cooldown: cooldown}
}

func label(k string, v float64) string {
	return k + itoa(int(v))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Headline rates (scaled for the laptop; frozen after calibration).
var (
	S0Rates = []float64{30}
	S1Rates = []float64{30, 60}
)

// S2 parameters: step 30 TPS every 15s from 30 up to 400 (about 13 steps worst case, ~3.5 min).
const (
	S2Start   = 30.0
	S2Step    = 30.0
	S2StepDur = 15
	S2Max     = 400.0
)
