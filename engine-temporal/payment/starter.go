package payment

import (
	"context"
	"errors"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/bill/ibps-bench/core/ingress"
	"github.com/bill/ibps-bench/core/model"
)

// Starter implements ingress.Starter on Temporal.
type Starter struct {
	Client       client.Client
	TaskQueue    string
	WorkflowName string
	Eager        bool // V1b: request eager start (needs a worker on the same client)
}

var _ ingress.Starter = (*Starter)(nil)

func (s *Starter) Start(ctx context.Context, in model.PaymentInput) error {
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       in.Payment.WorkflowID(),
		TaskQueue:                s.TaskQueue,
		EnableEagerStart:         s.Eager,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, s.WorkflowName, in)
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) { // a finished run exists for this payment: the duplicate is a no-op
		return nil
	}
	return err
}

func (s *Starter) Await(ctx context.Context, in model.PaymentInput) (model.Outcome, error) {
	var out model.Outcome
	err := s.Client.GetWorkflow(ctx, in.Payment.WorkflowID(), "").Get(ctx, &out)
	return out, err
}
