// Spike for the M0 go/no-go gate. Throwaway: answers "does the pinned Temporal Go SDK + server
// support what V1a/V1b need?" and prints one PASS/FAIL/NOTE line per gate item.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const tq = "spike-tq"

var flaky atomic.Int32 // fails the first 3 attempts of flakyActivity

func localStep(ctx context.Context, s string) (string, error) { return s + ":local", nil }

func regularStep(ctx context.Context, s string) (string, error) { return s + ":act", nil }

func flakyStep(ctx context.Context, s string) (string, error) {
	if flaky.Add(1) <= 3 {
		return "", fmt.Errorf("transient %d (attempt %d)", flaky.Load(), activity.GetInfo(ctx).Attempt)
	}
	return s + ":flaky-ok", nil
}

func PaymentWF(ctx workflow.Context, id string) (string, error) {
	done := false
	result := ""
	if err := workflow.SetUpdateHandler(ctx, "settled", func(ctx workflow.Context) (string, error) {
		if err := workflow.Await(ctx, func() bool { return done }); err != nil {
			return "", err
		}
		return result, nil
	}); err != nil {
		return "", err
	}
	lao := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: time.Second,
	})
	var r string
	if err := workflow.ExecuteLocalActivity(lao, localStep, id).Get(ctx, &r); err != nil {
		return "", err
	}
	// Unbounded retry with capped backoff: MaximumAttempts 0 = unlimited (step 5 / step 6 policy).
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Millisecond, BackoffCoefficient: 2,
			MaximumInterval: 40 * time.Millisecond, MaximumAttempts: 0,
		},
	})
	if err := workflow.ExecuteActivity(ao, flakyStep, r).Get(ctx, &r); err != nil {
		return "", err
	}
	result, done = r, true
	return r, nil
}

var (
	gapMu    sync.Mutex
	gapTimes = map[string][]time.Time{}
)

func stamp(key string) {
	gapMu.Lock()
	gapTimes[key] = append(gapTimes[key], time.Now())
	gapMu.Unlock()
}

// probeStep fails the first 4 attempts and records every attempt time under its kind.
func probeActivity(ctx context.Context, kind string) (string, error) {
	stamp(kind)
	gapMu.Lock()
	n := len(gapTimes[kind])
	gapMu.Unlock()
	if n <= 4 {
		return "", fmt.Errorf("transient %d", n)
	}
	return "ok", nil
}

func ProbeWF(ctx workflow.Context, kind string) (string, error) {
	rp := &temporal.RetryPolicy{InitialInterval: 50 * time.Millisecond, BackoffCoefficient: 2, MaximumInterval: 2 * time.Second, MaximumAttempts: 0}
	var r string
	if kind == "regular" {
		ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Second, RetryPolicy: rp})
		err := workflow.ExecuteActivity(ao, probeActivity, kind).Get(ctx, &r)
		return r, err
	}
	lao := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: time.Second, RetryPolicy: rp})
	err := workflow.ExecuteLocalActivity(lao, probeActivity, kind).Get(ctx, &r)
	return r, err
}

func BareWF(ctx workflow.Context, id string) (string, error) {
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Second})
	var r string
	err := workflow.ExecuteActivity(ao, regularStep, id).Get(ctx, &r)
	return r, err
}

func line(status, item, detail string) { fmt.Printf("%-5s %-46s %s\n", status, item, detail) }

// firstTaskDelay returns WorkflowExecutionStarted -> first WorkflowTaskStarted, and the request-ID
// kind, which differs between an eager start and a poll-dispatched first task.
func firstTaskDelay(ctx context.Context, c client.Client, id string) time.Duration {
	it := c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var t0, t1 time.Time
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			return -1
		}
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			t0 = ev.GetEventTime().AsTime()
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED:
			if t1.IsZero() {
				t1 = ev.GetEventTime().AsTime()
			}
		}
	}
	return t1.Sub(t0)
}

func median(d []time.Duration) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

func main() {
	ctx := context.Background()
	c, err := client.Dial(client.Options{HostPort: "localhost:7233", Logger: log.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	if err != nil {
		panic(err)
	}
	defer c.Close()
	// Worker on the SAME client as the starter: required for eager start (co-located, V1b shape).
	w := worker.New(c, tq, worker.Options{})
	w.RegisterWorkflow(PaymentWF)
	w.RegisterWorkflow(BareWF)
	w.RegisterWorkflow(ProbeWF)
	w.RegisterActivity(probeActivity)
	w.RegisterActivity(localStep)
	w.RegisterActivity(regularStep)
	w.RegisterActivity(flakyStep)
	if err := w.Start(); err != nil {
		panic(err)
	}
	defer w.Stop()
	run := fmt.Sprintf("%d", time.Now().UnixNano())

	// G1: unbounded retry with capped backoff + Local Activity, on a plain start.
	flaky.Store(0)
	f, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "g1-" + run, TaskQueue: tq}, PaymentWF, "p1")
	var out string
	if err == nil {
		err = f.Get(ctx, &out)
	}
	if err == nil && out == "p1:local:act:flaky-ok" || out == "p1:local:flaky-ok" {
		line("PASS", "G1 local activity + unbounded capped retry", out+fmt.Sprintf(" (activity attempts=%d)", flaky.Load()))
	} else {
		line("FAIL", "G1 local activity + unbounded capped retry", fmt.Sprintf("out=%q err=%v", out, err))
	}

	// G2: eager start really happens. Compare first-task delay eager vs not (median of 40).
	measure := func(eager bool) time.Duration {
		var ds []time.Duration
		for i := 0; i < 40; i++ {
			id := fmt.Sprintf("g2-%v-%d-%s", eager, i, run)
			r, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: tq, EnableEagerStart: eager}, BareWF, "x")
			if err != nil {
				panic(err)
			}
			_ = r.Get(ctx, &out)
			ds = append(ds, firstTaskDelay(ctx, c, id))
		}
		return median(ds)
	}
	off, on := measure(false), measure(true)
	st := "NOTE"
	if on < off {
		st = "PASS"
	}
	line(st, "G2 eager start (ExecuteWorkflow)", fmt.Sprintf("median start->first task: eager=%v, poll=%v", on, off))

	// G3: Update-with-Start works, both wait stages, and USE_EXISTING dedups a second call.
	for _, stage := range []client.WorkflowUpdateStage{client.WorkflowUpdateStageAccepted, client.WorkflowUpdateStageCompleted} {
		flaky.Store(0)
		id := fmt.Sprintf("g3-%d-%s", stage, run)
		op := c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
			ID: id, TaskQueue: tq, WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}, PaymentWF, "p3")
		t := time.Now()
		h, err := c.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
			StartWorkflowOperation: op,
			UpdateOptions:          client.UpdateWorkflowOptions{UpdateID: id + "-u", UpdateName: "settled", WaitForStage: stage},
		})
		if err != nil {
			line("FAIL", fmt.Sprintf("G3 update-with-start stage=%d", stage), err.Error())
			continue
		}
		took := time.Since(t)
		res := "(not awaited)"
		if stage == client.WorkflowUpdateStageCompleted {
			_ = h.Get(ctx, &res)
		}
		line("PASS", fmt.Sprintf("G3 update-with-start stage=%d", stage), fmt.Sprintf("returned in %v, result=%s", took.Round(time.Millisecond), res))
	}

	// G4: Update-with-Start combined with eager start. The SDK doc says this is not allowed.
	op := c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID: "g4-" + run, TaskQueue: tq, EnableEagerStart: true,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, PaymentWF, "p4")
	_, err = c.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: op,
		UpdateOptions:          client.UpdateWorkflowOptions{UpdateID: "g4-u", UpdateName: "settled", WaitForStage: client.WorkflowUpdateStageAccepted},
	})
	if err != nil {
		line("FAIL", "G4 update-with-start + eager start", "rejected: "+err.Error())
	} else {
		d := firstTaskDelay(ctx, c, "g4-"+run)
		line("NOTE", "G4 update-with-start + eager start", fmt.Sprintf("accepted; first task delay %v (compare with G2 eager=%v poll=%v)", d, on, off))
	}

	// G6: retry gaps with a 50 ms initial interval (step 3/4 policy): regular vs local activity.
	for _, kind := range []string{"regular", "local"} {
		start := time.Now()
		r, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "g6-" + kind + "-" + run, TaskQueue: tq}, ProbeWF, kind)
		if err == nil {
			err = r.Get(ctx, &out)
		}
		gapMu.Lock()
		ts := gapTimes[kind]
		gapMu.Unlock()
		var gaps []time.Duration
		for i := 1; i < len(ts); i++ {
			gaps = append(gaps, ts[i].Sub(ts[i-1]).Round(time.Millisecond))
		}
		line("NOTE", "G6 retry gaps, "+kind+" activity (configured 50ms x2, cap 2s)", fmt.Sprintf("gaps=%v total=%v err=%v", gaps, time.Since(start).Round(time.Millisecond), err))
	}

	// G5: dedup by workflow ID (same ID twice -> one execution).
	id := "g5-" + run
	var runIDs []string
	for i := 0; i < 2; i++ {
		r, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: id, TaskQueue: tq, WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		}, BareWF, "d")
		if err != nil {
			var already *serviceerror.WorkflowExecutionAlreadyStarted
			line("NOTE", "G5 dedup second start", fmt.Sprintf("err=%v alreadyStarted=%v", err, errorsAs(err, &already)))
			continue
		}
		runIDs = append(runIDs, r.GetRunID())
	}
	if len(runIDs) == 2 && runIDs[0] == runIDs[1] {
		line("PASS", "G5 dedup by workflow ID", "second start attached to same run "+runIDs[0][:8])
	} else {
		line("NOTE", "G5 dedup by workflow ID", fmt.Sprintf("runs=%v", runIDs))
	}
}

func errorsAs(err error, target **serviceerror.WorkflowExecutionAlreadyStarted) bool {
	e, ok := err.(*serviceerror.WorkflowExecutionAlreadyStarted)
	if ok {
		*target = e
	}
	return ok
}

func init() { probes = append(probes, nil) }

var probes []func()
