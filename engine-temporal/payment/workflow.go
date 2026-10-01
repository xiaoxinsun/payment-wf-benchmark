// Package payment holds the Temporal binding of the shared business core: the workflow, the activity
// wrappers and the ingress starter. All business logic lives in core/steps; this file is orchestration only.
package payment

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

// Workflow type names. V1a uses regular activities everywhere; V1b runs steps 3, 4 and RecordState as
// Local Activities (step 4b, 5 and 6 stay regular so they can wait or retry indefinitely).
const (
	WorkflowA = "PaymentWorkflowA"
	WorkflowB = "PaymentWorkflowB"
)

// Cfg is set once at process start, before any worker or workflow runs. It never changes at runtime.
var Cfg config.Config

// LocalActs is the instance Local Activities run on (V1b). Local Activities are invoked by function
// reference inside the worker process, so the reference must be bound to a real instance. It is only
// touched when the workflow is the V1b type; regular activities are called by registered name.
var LocalActs *Activities

func PaymentWorkflowA(ctx workflow.Context, in model.PaymentInput) (model.Outcome, error) {
	return run(ctx, in, false)
}

func PaymentWorkflowB(ctx workflow.Context, in model.PaymentInput) (model.Outcome, error) {
	return run(ctx, in, true)
}

func retryPolicy(p config.RetryPolicy) *temporal.RetryPolicy {
	return &temporal.RetryPolicy{
		InitialInterval: p.Initial, BackoffCoefficient: p.Factor, MaximumInterval: p.MaxInterval,
		MaximumAttempts: int32(p.MaxAttempts), // 0 = unbounded
	}
}

// slack keeps Temporal's own attempt timeout above the HTTP timeout enforced inside the step, so the
// step's deadline is what decides an attempt, identically in every engine.
const slack = 250 * time.Millisecond

func run(ctx workflow.Context, in model.PaymentInput, local bool) (model.Outcome, error) {
	c := Cfg
	// call runs one shared-core step: by activity name (regular) or by bound function (Local Activity).
	// Bounded steps stop at the SLA deadline.
	call := func(name string, localFn any, timeout time.Duration, rp config.RetryPolicy, isLocal bool, args ...any) workflow.Future {
		var sched time.Duration
		if rp.BoundedBySLA {
			sched = in.Deadline.Sub(workflow.Now(ctx))
			if sched < time.Millisecond {
				sched = time.Millisecond
			}
		}
		if isLocal {
			lao := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
				StartToCloseTimeout: timeout + slack, ScheduleToCloseTimeout: sched, RetryPolicy: retryPolicy(rp)})
			return workflow.ExecuteLocalActivity(lao, localFn, args...)
		}
		ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: timeout + slack, ScheduleToCloseTimeout: sched, RetryPolicy: retryPolicy(rp)})
		return workflow.ExecuteActivity(ao, name, args...)
	}
	recordPolicy := config.RetryPolicy{Initial: 50 * time.Millisecond, Factor: 2, MaxInterval: 2 * time.Second}
	record := func(state model.PaymentState, reason model.RejectReason, at time.Time) error {
		return call("Record", LocalActs.Record, 5*time.Second, recordPolicy, local, in, state, reason, at).Get(ctx, nil)
	}
	respond := func(o model.Outcome) error {
		return call("Respond", nil, c.Timeouts.Step6Respond, c.Retries.Step6Respond, false, in, o).Get(ctx, nil)
	}
	reject := func(reason model.RejectReason, at time.Time) (model.Outcome, error) {
		o := model.Outcome{Reason: reason, DecidedAt: at}
		if err := record(model.StateRejected, reason, at); err != nil {
			return o, err
		}
		if err := respond(o); err != nil {
			return o, err
		}
		return o, record(model.StateRejectedSent, reason, time.Time{})
	}

	// Step 2: pure checks, no activity.
	if reason, bad := steps.Check(c.SLA, in.Payment); bad {
		return reject(reason, in.ReceivedAt)
	}

	// Step 3: validate. Retries exhausted (inside the SLA) means the SLA is gone: reject AB05.
	var v steps.ValidateResult
	if err := call("Validate", LocalActs.Validate, c.Timeouts.Step3Validate, c.Retries.Step3Validate, local, in).Get(ctx, &v); err != nil {
		return reject(model.ReasonSLATimeout, workflow.Now(ctx))
	}
	if v.Reject != "" {
		return reject(v.Reject, v.DecidedAt)
	}
	if err := record(model.StateValidated, "", time.Time{}); err != nil {
		return model.Outcome{}, err
	}

	// Step 4: AML screening, and step 4b when the result is REVIEW.
	var s steps.ScreenResult
	if err := call("Screen", LocalActs.Screen, c.Timeouts.Step4Screen, c.Retries.Step4Screen, local, in).Get(ctx, &s); err != nil {
		return reject(model.ReasonSLATimeout, workflow.Now(ctx))
	}
	if s.Verdict == "REJECT" {
		return reject(s.Reject, s.DecidedAt)
	}
	if s.Verdict == "REVIEW" {
		var r steps.ReviewResult
		remaining := in.Deadline.Sub(workflow.Now(ctx)) + time.Second
		ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: remaining, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: 50 * time.Millisecond}})
		if err := workflow.ExecuteActivity(ao, "Review", in, s.CaseID).Get(ctx, &r); err != nil {
			return reject(model.ReasonAMLReviewTimeout, workflow.Now(ctx))
		}
		if r.Reject != "" {
			return reject(r.Reject, r.DecidedAt)
		}
	}
	if steps.CodeFault(in.Payment.MsgID) {
		panic("C1 injected code fault in the workflow body") // probe: unexpected exception outside any activity
	}
	if err := record(model.StateScreened, "", time.Time{}); err != nil {
		return model.Outcome{}, err
	}

	// Step 5: settlement. Point of no return: retried until it succeeds, no SLA bound.
	var p steps.PostResult
	if err := call("Post", nil, c.Timeouts.Step5Post+c.Timeouts.Step5StatusQuery, c.Retries.Step5Post, false, in).Get(ctx, &p); err != nil {
		return model.Outcome{}, err
	}
	postedAt := p.PostedAt
	if postedAt.IsZero() {
		postedAt = workflow.Now(ctx)
	}
	if err := record(model.StateSettled, "", postedAt); err != nil {
		return model.Outcome{}, err
	}

	// Step 6: ACCP receipt, unbounded retry.
	o := model.Outcome{Accepted: true, DecidedAt: postedAt}
	if err := respond(o); err != nil {
		return o, err
	}
	return o, record(model.StateAcceptedSent, "", time.Time{})
}
