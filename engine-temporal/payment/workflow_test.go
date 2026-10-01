package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

func init() {
	c, err := config.Load("../../deploy/config")
	if err != nil {
		panic(err)
	}
	Cfg = c
}

// rig runs the workflow in Temporal's test environment against overridable fake activities.
// (testify uses the first matching expectation, so behaviour is swapped through fields, not re-mocked.)
type rig struct {
	env *testsuite.TestWorkflowEnvironment
	in  model.PaymentInput

	mu    sync.Mutex
	calls []string

	validate func() (steps.ValidateResult, error)
	screen   func() (steps.ScreenResult, error)
	review   func() (steps.ReviewResult, error)
	post     func() (steps.PostResult, error)
	respond  func(model.Outcome) error
}

func (r *rig) add(s string) { r.mu.Lock(); r.calls = append(r.calls, s); r.mu.Unlock() }

func (r *rig) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == s {
			n++
		}
	}
	return n
}

func newRig(t *testing.T) *rig {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	now := time.Now()
	env.SetStartTime(now)
	a := &Activities{}
	LocalActs = a
	env.RegisterActivity(a)
	p := model.Payment{MsgID: "M1", EndToEndID: "E1", InstgAgent: "102100099996", InstdAgent: Cfg.SLA.OurBankCode,
		AmountFen: 1000, Currency: "CNY", CreditorAcct: "6222000000000042", CreditorName: "C", DebtorName: "D", CreDtTm: now}
	r := &rig{env: env, in: model.PaymentInput{Payment: p, ReceivedAt: now, Deadline: now.Add(Cfg.SLA.Budget)}}
	r.validate = func() (steps.ValidateResult, error) { return steps.ValidateResult{DecidedAt: now}, nil }
	r.screen = func() (steps.ScreenResult, error) { return steps.ScreenResult{Verdict: "CLEAR", DecidedAt: now}, nil }
	r.post = func() (steps.PostResult, error) { return steps.PostResult{PostedAt: now}, nil }
	r.respond = func(model.Outcome) error { return nil }

	env.OnActivity(a.Validate, mock.Anything, mock.Anything).Return(func(_ context.Context, _ model.PaymentInput) (steps.ValidateResult, error) {
		r.add("validate")
		return r.validate()
	})
	env.OnActivity(a.Screen, mock.Anything, mock.Anything).Return(func(_ context.Context, _ model.PaymentInput) (steps.ScreenResult, error) {
		r.add("screen")
		return r.screen()
	})
	env.OnActivity(a.Review, mock.Anything, mock.Anything, mock.Anything).Return(func(_ context.Context, _ model.PaymentInput, _ string) (steps.ReviewResult, error) {
		r.add("review")
		return r.review()
	})
	env.OnActivity(a.Post, mock.Anything, mock.Anything).Return(func(_ context.Context, _ model.PaymentInput) (steps.PostResult, error) {
		r.add("post")
		return r.post()
	})
	env.OnActivity(a.Respond, mock.Anything, mock.Anything, mock.Anything).Return(func(_ context.Context, _ model.PaymentInput, o model.Outcome) error {
		if o.Accepted {
			r.add("respond:ACCP")
		} else {
			r.add("respond:" + o.Reason.ISOCode())
		}
		return r.respond(o)
	})
	env.OnActivity(a.Record, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ model.PaymentInput, s model.PaymentState, _ model.RejectReason, _ time.Time) error {
			r.add("record:" + string(s))
			return nil
		})
	return r
}

func (r *rig) exec(local bool) (model.Outcome, error) {
	if local {
		r.env.ExecuteWorkflow(PaymentWorkflowB, r.in)
	} else {
		r.env.ExecuteWorkflow(PaymentWorkflowA, r.in)
	}
	var out model.Outcome
	if !r.env.IsWorkflowCompleted() {
		return out, errors.New("workflow did not complete")
	}
	if err := r.env.GetWorkflowError(); err != nil {
		return out, err
	}
	return out, r.env.GetWorkflowResult(&out)
}

func forBoth(t *testing.T, f func(t *testing.T, r *rig, local bool)) {
	t.Run("V1a", func(t *testing.T) { f(t, newRig(t), false) })
	t.Run("V1b", func(t *testing.T) { f(t, newRig(t), true) })
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls:\n got %v\nwant %v", got, want)
		}
	}
}

func TestHappyPath(t *testing.T) {
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		out, err := r.exec(local)
		if err != nil || !out.Accepted {
			t.Fatalf("%+v %v", out, err)
		}
		assertOrder(t, r.calls, "validate", "record:VALIDATED", "screen", "record:SCREENED", "post",
			"record:SETTLED", "respond:ACCP", "record:ACCEPTED_SENT")
	})
}

func TestStep2RejectsMakeNoExternalCall(t *testing.T) {
	cases := map[string]func(*model.Payment){
		"AM03": func(p *model.Payment) { p.Currency = "USD" },
		"AM02": func(p *model.Payment) { p.AmountFen = Cfg.SLA.AmountLimitFen + 1 },
		"AGNT": func(p *model.Payment) { p.InstdAgent = "000000000000" },
	}
	for code, edit := range cases {
		forBoth(t, func(t *testing.T, r *rig, local bool) {
			edit(&r.in.Payment)
			out, err := r.exec(local)
			if err != nil || out.Accepted || out.Reason.ISOCode() != code {
				t.Fatalf("%s: %+v %v", code, out, err)
			}
			assertOrder(t, r.calls, "record:REJECTED", "respond:"+code, "record:REJECTED_SENT")
		})
	}
}

func TestValidateAndAMLRejectsNeverPost(t *testing.T) {
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.validate = func() (steps.ValidateResult, error) {
			return steps.ValidateResult{Reject: model.ReasonAccountClosed, DecidedAt: time.Now()}, nil
		}
		out, err := r.exec(local)
		if err != nil || out.Reason != model.ReasonAccountClosed {
			t.Fatalf("%+v %v", out, err)
		}
		assertOrder(t, r.calls, "validate", "record:REJECTED", "respond:AC04", "record:REJECTED_SENT")
	})
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.screen = func() (steps.ScreenResult, error) {
			return steps.ScreenResult{Verdict: "REJECT", Reject: model.ReasonAMLHit, DecidedAt: time.Now()}, nil
		}
		out, err := r.exec(local)
		if err != nil || out.Reason != model.ReasonAMLHit || r.count("post") != 0 {
			t.Fatalf("%+v %v %v", out, err, r.calls)
		}
	})
}

func TestReviewPaths(t *testing.T) {
	reviewing := func(r *rig, res steps.ReviewResult, err error) {
		r.screen = func() (steps.ScreenResult, error) {
			return steps.ScreenResult{Verdict: "REVIEW", CaseID: "C1", DecidedAt: time.Now()}, nil
		}
		r.review = func() (steps.ReviewResult, error) { return res, err }
	}
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		reviewing(r, steps.ReviewResult{DecidedAt: time.Now()}, nil)
		if out, err := r.exec(local); err != nil || !out.Accepted || r.count("post") != 1 || r.count("review") != 1 {
			t.Fatalf("cleared review: %+v %v %v", out, err, r.calls)
		}
	})
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		reviewing(r, steps.ReviewResult{Reject: model.ReasonAMLReviewTimeout, DecidedAt: time.Now()}, nil)
		if out, err := r.exec(local); err != nil || out.Reason.ISOCode() != "RR04" || r.count("post") != 0 {
			t.Fatalf("review timeout: %+v %v %v", out, err, r.calls)
		}
	})
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		reviewing(r, steps.ReviewResult{}, errors.New("boom"))
		if out, err := r.exec(local); err != nil || out.Reason != model.ReasonAMLReviewTimeout || r.count("post") != 0 {
			t.Fatalf("review activity failure: %+v %v %v", out, err, r.calls)
		}
	})
}

func TestRetriesExhaustedInsideSLARejectsAB05(t *testing.T) {
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.validate = func() (steps.ValidateResult, error) { return steps.ValidateResult{}, errors.New("cbs 503") }
		out, err := r.exec(local)
		if err != nil || out.Reason != model.ReasonSLATimeout || r.count("post") != 0 {
			t.Fatalf("%+v %v %v", out, err, r.calls)
		}
		if n := r.count("validate"); n != Cfg.Retries.Step3Validate.MaxAttempts {
			t.Errorf("validate attempts %d, want %d", n, Cfg.Retries.Step3Validate.MaxAttempts)
		}
	})
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.screen = func() (steps.ScreenResult, error) { return steps.ScreenResult{}, errors.New("aml down") }
		out, err := r.exec(local)
		if err != nil || out.Reason != model.ReasonSLATimeout || r.count("post") != 0 {
			t.Fatalf("aml: %+v %v %v", out, err, r.calls)
		}
	})
}

func TestSettlementAndReceiptRetryUntilSuccess(t *testing.T) {
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.post = func() (steps.PostResult, error) {
			if r.count("post") <= 6 { // more than any bounded policy would allow
				return steps.PostResult{}, errors.New("cbs timeout")
			}
			return steps.PostResult{PostedAt: time.Now()}, nil
		}
		r.respond = func(model.Outcome) error {
			if r.count("respond:ACCP") <= 5 {
				return errors.New("npc down")
			}
			return nil
		}
		out, err := r.exec(local)
		if err != nil || !out.Accepted || r.count("post") != 7 || r.count("respond:ACCP") != 6 {
			t.Fatalf("%+v %v posts=%d sends=%d", out, err, r.count("post"), r.count("respond:ACCP"))
		}
	})
}

func TestPostAfterSLAExpiredStillSettles(t *testing.T) {
	// Settlement is the point of no return: with the SLA long gone the payment is still settled and ACCP sent.
	forBoth(t, func(t *testing.T, r *rig, local bool) {
		r.in.Deadline = r.in.ReceivedAt.Add(-time.Hour)
		out, err := r.exec(local)
		if err != nil || !out.Accepted || r.count("post") != 1 {
			t.Fatalf("%+v %v %v", out, err, r.calls)
		}
	})
}
