package payment

import (
	"context"
	"time"

	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

// Activities adapts core/steps to Temporal activities. Method names are the registered activity names.
type Activities struct{ Deps *steps.Deps }

func (a *Activities) Validate(ctx context.Context, in model.PaymentInput) (steps.ValidateResult, error) {
	return a.Deps.ValidateAccount(ctx, in)
}

func (a *Activities) Screen(ctx context.Context, in model.PaymentInput) (steps.ScreenResult, error) {
	return a.Deps.ScreenAML(ctx, in)
}

func (a *Activities) Review(ctx context.Context, in model.PaymentInput, caseID string) (steps.ReviewResult, error) {
	return a.Deps.AwaitReview(ctx, in, caseID)
}

func (a *Activities) Post(ctx context.Context, in model.PaymentInput) (steps.PostResult, error) {
	return a.Deps.PostSettlement(ctx, in)
}

func (a *Activities) Respond(ctx context.Context, in model.PaymentInput, o model.Outcome) error {
	return a.Deps.SendReceipt(ctx, in, o)
}

func (a *Activities) Record(ctx context.Context, in model.PaymentInput, state model.PaymentState, reason model.RejectReason, at time.Time) error {
	return a.Deps.RecordState(ctx, in, state, reason, at)
}
