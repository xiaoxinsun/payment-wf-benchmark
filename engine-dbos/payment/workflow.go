// Package payment holds the DBOS binding of the shared business core: the workflow, the step wrappers and
// the ingress starter. All business logic lives in core/steps; this file is orchestration only, and is the
// same control flow as engine-temporal/payment/workflow.go.
package payment

import (
	"context"
	"math"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

// WorkflowName is fixed so recovered workflows resolve to the same function after a restart.
const WorkflowName = "PaymentWorkflow"

// Core is the shared business core as the workflow sees it (implemented by *steps.Deps).
type Core interface {
	ValidateAccount(ctx context.Context, in model.PaymentInput) (steps.ValidateResult, error)
	ScreenAML(ctx context.Context, in model.PaymentInput) (steps.ScreenResult, error)
	AwaitReview(ctx context.Context, in model.PaymentInput, caseID string) (steps.ReviewResult, error)
	PostSettlement(ctx context.Context, in model.PaymentInput) (steps.PostResult, error)
	SendReceipt(ctx context.Context, in model.PaymentInput, o model.Outcome) error
	RecordState(ctx context.Context, in model.PaymentInput, state model.PaymentState, reason model.RejectReason, decidedAt time.Time) error
}

var _ Core = (*steps.Deps)(nil)

// Impl and Cfg are set once at process start, before Launch.
var (
	Impl Core
	Cfg  config.Config
)

// unbounded is the DBOS spelling of "retry until it succeeds" (the SDK has an int max-retries).
const unbounded = math.MaxInt32

func opts(name string, p config.RetryPolicy) []dbos.StepOption {
	max := p.MaxAttempts - 1 // DBOS counts retries after the first attempt
	if p.MaxAttempts == 0 {
		max = unbounded
	}
	return []dbos.StepOption{
		dbos.WithStepName(name), dbos.WithStepMaxRetries(max), dbos.WithStepBaseInterval(p.Initial),
		dbos.WithStepBackoffFactor(p.Factor), dbos.WithStepMaxInterval(p.MaxInterval),
	}
}

func step[R any](ctx dbos.Context, name string, p config.RetryPolicy, fn func(context.Context) (R, error)) (R, error) {
	return dbos.RunAsStep(ctx, fn, opts(name, p)...)
}

// Workflow is the inward payment flow.
func Workflow(ctx dbos.Context, in model.PaymentInput) (model.Outcome, error) {
	c := Cfg
	recordPolicy := config.RetryPolicy{Initial: 50 * time.Millisecond, Factor: 2, MaxInterval: 2 * time.Second}
	record := func(name string, state model.PaymentState, reason model.RejectReason, at time.Time) error {
		_, err := step(ctx, name, recordPolicy, func(sc context.Context) (struct{}, error) {
			return struct{}{}, Impl.RecordState(sc, in, state, reason, at)
		})
		return err
	}
	respond := func(o model.Outcome) error {
		_, err := step(ctx, "respond", c.Retries.Step6Respond, func(sc context.Context) (struct{}, error) {
			return struct{}{}, Impl.SendReceipt(sc, in, o)
		})
		return err
	}
	reject := func(reason model.RejectReason, at time.Time) (model.Outcome, error) {
		o := model.Outcome{Reason: reason, DecidedAt: at}
		if err := record("record-rejected", model.StateRejected, reason, at); err != nil {
			return o, err
		}
		if err := respond(o); err != nil {
			return o, err
		}
		return o, record("record-rejected-sent", model.StateRejectedSent, reason, time.Time{})
	}
	// A durable timestamp for decisions the workflow makes itself (retries exhausted). It is captured in a
	// step so a replay reads the same value; DBOS workflow bodies must not call time.Now directly.
	now := func(name string) time.Time {
		t, _ := step(ctx, name, recordPolicy, func(context.Context) (time.Time, error) { return time.Now(), nil })
		return t
	}

	// Step 2: pure checks, no step.
	if reason, bad := steps.Check(c.SLA, in.Payment); bad {
		return reject(reason, in.ReceivedAt)
	}

	// Step 3: validate. Retries exhausted inside the SLA means the SLA is gone: reject AB05.
	v, err := step(ctx, "validate", c.Retries.Step3Validate, func(sc context.Context) (steps.ValidateResult, error) {
		return Impl.ValidateAccount(sc, in)
	})
	if err != nil {
		return reject(model.ReasonSLATimeout, now("now-validate"))
	}
	if v.Reject != "" {
		return reject(v.Reject, v.DecidedAt)
	}
	if err := record("record-validated", model.StateValidated, "", time.Time{}); err != nil {
		return model.Outcome{}, err
	}

	// Step 4: AML, and 4b when REVIEW.
	s, err := step(ctx, "screen", c.Retries.Step4Screen, func(sc context.Context) (steps.ScreenResult, error) {
		return Impl.ScreenAML(sc, in)
	})
	if err != nil {
		return reject(model.ReasonSLATimeout, now("now-screen"))
	}
	if s.Verdict == "REJECT" {
		return reject(s.Reject, s.DecidedAt)
	}
	if s.Verdict == "REVIEW" {
		r, err := step(ctx, "review", config.RetryPolicy{Initial: 50 * time.Millisecond, Factor: 2, MaxInterval: 2 * time.Second, MaxAttempts: 3},
			func(sc context.Context) (steps.ReviewResult, error) { return Impl.AwaitReview(sc, in, s.CaseID) })
		if err != nil {
			return reject(model.ReasonAMLReviewTimeout, now("now-review"))
		}
		if r.Reject != "" {
			return reject(r.Reject, r.DecidedAt)
		}
	}
	if steps.CodeFault(in.Payment.MsgID) {
		panic("C1 injected code fault in the workflow body") // probe: unexpected exception outside any step
	}
	if err := record("record-screened", model.StateScreened, "", time.Time{}); err != nil {
		return model.Outcome{}, err
	}

	// Step 5: settlement. Point of no return: retried until it succeeds, no SLA bound.
	p, err := step(ctx, "post", c.Retries.Step5Post, func(sc context.Context) (steps.PostResult, error) {
		return Impl.PostSettlement(sc, in)
	})
	if err != nil {
		return model.Outcome{}, err
	}
	postedAt := p.PostedAt
	if postedAt.IsZero() {
		postedAt = now("now-post")
	}
	if err := record("record-settled", model.StateSettled, "", postedAt); err != nil {
		return model.Outcome{}, err
	}

	// Step 6: ACCP receipt, unbounded retry.
	o := model.Outcome{Accepted: true, DecidedAt: postedAt}
	if err := respond(o); err != nil {
		return o, err
	}
	return o, record("record-accepted-sent", model.StateAcceptedSent, "", time.Time{})
}

// Register registers the workflow. Call before dbos.Launch.
func Register(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, Workflow, dbos.WithWorkflowName(WorkflowName), dbos.WithMaxRecoveryAttempts(math.MaxInt32))
}
