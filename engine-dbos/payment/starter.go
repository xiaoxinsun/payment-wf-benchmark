package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/bill/ibps-bench/core/ingress"
	"github.com/bill/ibps-bench/core/model"
)

// Starter implements ingress.Starter on DBOS: it runs the workflow directly in this process with the
// payment's dedup key as workflow ID. A semaphore caps in-flight workflows per replica.
type Starter struct {
	Ctx      dbos.Context
	sem      chan struct{}
	WaitSlot time.Duration
}

var _ ingress.Starter = (*Starter)(nil)

func NewStarter(ctx dbos.Context, inflightCap int, waitSlot time.Duration) *Starter {
	return &Starter{Ctx: ctx, sem: make(chan struct{}, inflightCap), WaitSlot: waitSlot}
}

func (s *Starter) Start(ctx context.Context, in model.PaymentInput) error {
	select {
	case s.sem <- struct{}{}:
	case <-time.After(s.WaitSlot):
		return fmt.Errorf("replica at its in-flight cap (%d)", cap(s.sem))
	case <-ctx.Done():
		return ctx.Err()
	}
	h, err := dbos.RunWorkflow(s.Ctx, Workflow, in, dbos.WithWorkflowID(in.Payment.WorkflowID()))
	if err != nil {
		<-s.sem
		return err
	}
	go func() {
		defer func() { <-s.sem }()
		_, _ = h.GetResult()
	}()
	return nil
}

func (s *Starter) Await(ctx context.Context, in model.PaymentInput) (model.Outcome, error) {
	h, err := dbos.RetrieveWorkflow[model.Outcome](s.Ctx, in.Payment.WorkflowID())
	if err != nil {
		return model.Outcome{}, err
	}
	return h.GetResult()
}
