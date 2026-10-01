// Spike for the M0 go/no-go gate. Throwaway: answers "does the pinned DBOS Go SDK support what V2
// needs?" and prints one PASS/FAIL/NOTE line per gate item.
//
//	go run . basic                      D1-D4 in one process
//	go run . own  <executorID>          start a workflow whose step blocks 60s (for the kill test)
//	go run . watch <executorID> <secs>  launch and report the status of workflow "wf-orphan"
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

const dbURL = "postgres://dbos:dbos@localhost:5433/dbos_sys?sslmode=disable"

func line(status, item, detail string) { fmt.Printf("%-5s %-50s %s\n", status, item, detail) }

func newCtx(exec string) dbos.Context {
	if exec != "" {
		os.Setenv("DBOS__VMID", exec)
	}
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName: "spike", DatabaseURL: dbURL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		panic(err)
	}
	return ctx
}

var (
	execCount atomic.Int32
	gapMu     sync.Mutex
	gapTimes  []time.Time
	inFlight  atomic.Int32
	maxInFl   atomic.Int32
)

func retryStep(ctx context.Context) (string, error) {
	gapMu.Lock()
	gapTimes = append(gapTimes, time.Now())
	n := len(gapTimes)
	gapMu.Unlock()
	if n <= 4 {
		return "", fmt.Errorf("transient %d", n)
	}
	return "ok", nil
}

func permanentStep(ctx context.Context) (string, error) {
	execCount.Add(1)
	return "", fmt.Errorf("business reject (non-retryable)")
}

func RetryWF(ctx dbos.Context, _ string) (string, error) {
	// step 3/4 policy: 50 ms x2 backoff. Step 5/6 policy: effectively unbounded, cap 2 s.
	return dbos.RunAsStep(ctx, retryStep,
		dbos.WithStepMaxRetries(1<<30), dbos.WithStepBaseInterval(50*time.Millisecond),
		dbos.WithStepBackoffFactor(2), dbos.WithStepMaxInterval(2*time.Second))
}

func RejectWF(ctx dbos.Context, _ string) (string, error) {
	return dbos.RunAsStep(ctx, permanentStep, dbos.WithStepMaxRetries(3),
		dbos.WithStepRetryPredicate(func(error) bool { return false }))
}

func IdemWF(ctx dbos.Context, in string) (string, error) {
	return dbos.RunAsStep(ctx, func(context.Context) (string, error) { execCount.Add(1); return in + "-done", nil })
}

func QueuedWF(ctx dbos.Context, _ string) (string, error) {
	return dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFl.Load()
			if n <= m || maxInFl.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return "q", nil
	})
}

func TimeoutWF(ctx dbos.Context, _ string) (string, error) {
	return dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		select {
		case <-time.After(2 * time.Second):
			return "finished", nil
		case <-c.Done():
			return "", fmt.Errorf("attempt deadline: %w", c.Err())
		}
	})
}

func OrphanWF(ctx dbos.Context, _ string) (string, error) {
	return dbos.RunAsStep(ctx, func(context.Context) (string, error) {
		time.Sleep(60 * time.Second)
		return "orphan-done", nil
	})
}

func basic() {
	ctx := newCtx("")
	q, err := dbos.RegisterQueue(ctx, "spike-q", dbos.WithWorkerConcurrency(2))
	if err != nil {
		panic(err)
	}
	dbos.RegisterWorkflow(ctx, RetryWF)
	dbos.RegisterWorkflow(ctx, RejectWF)
	dbos.RegisterWorkflow(ctx, IdemWF)
	dbos.RegisterWorkflow(ctx, QueuedWF)
	dbos.RegisterWorkflow(ctx, TimeoutWF)
	if err := dbos.Launch(ctx); err != nil {
		panic(err)
	}
	defer dbos.Shutdown(ctx, 2*time.Second)
	run := strconv.FormatInt(time.Now().UnixNano(), 10)

	// D1: large max-retries with capped exponential backoff.
	start := time.Now()
	h, err := dbos.RunWorkflow(ctx, RetryWF, "", dbos.WithWorkflowID("d1-"+run))
	var out string
	if err == nil {
		out, err = h.GetResult()
	}
	var gaps []time.Duration
	for i := 1; i < len(gapTimes); i++ {
		gaps = append(gaps, gapTimes[i].Sub(gapTimes[i-1]).Round(time.Millisecond))
	}
	st := "PASS"
	if err != nil {
		st = "FAIL"
	}
	line(st, "D1 step retry 50ms x2, max-retries=2^30", fmt.Sprintf("out=%q gaps=%v total=%v err=%v", out, gaps, time.Since(start).Round(time.Millisecond), err))

	// D1b: non-retryable business error stops immediately (predicate).
	h2, _ := dbos.RunWorkflow(ctx, RejectWF, "", dbos.WithWorkflowID("d1b-"+run))
	_, err = h2.GetResult()
	st = "FAIL"
	if err != nil && execCount.Load() == 1 {
		st = "PASS"
	}
	line(st, "D1b non-retryable error via predicate", fmt.Sprintf("attempts=%d err=%v", execCount.Load(), err))

	// D2: per-attempt timeout via context deadline on the step.
	tctx, cancel := dbos.WithTimeout(ctx, 300*time.Millisecond)
	h3, err := dbos.RunWorkflow(tctx, TimeoutWF, "", dbos.WithWorkflowID("d2-"+run))
	if err == nil {
		out, err = h3.GetResult()
	}
	cancel()
	line("NOTE", "D2 per-step timeout", fmt.Sprintf("out=%q err=%v (no per-step timeout option; ctx timeout applies to the whole workflow)", out, err))

	// D3: workflow ID as idempotency key.
	execCount.Store(0)
	var ids []string
	for i := 0; i < 2; i++ {
		h4, err := dbos.RunWorkflow(ctx, IdemWF, "x", dbos.WithWorkflowID("d3-"+run))
		if err != nil {
			line("FAIL", "D3 workflow-ID dedup", err.Error())
			return
		}
		r, _ := h4.GetResult()
		ids = append(ids, h4.GetWorkflowID()+"="+r)
	}
	st = "FAIL"
	if execCount.Load() == 1 {
		st = "PASS"
	}
	line(st, "D3 workflow-ID dedup (same ID twice)", fmt.Sprintf("step executions=%d results=%v", execCount.Load(), ids))

	// D4: queue worker concurrency cap.
	var hs []dbos.WorkflowHandle[string]
	for i := 0; i < 12; i++ {
		h5, err := dbos.RunWorkflow(ctx, QueuedWF, "", dbos.WithQueue(q), dbos.WithWorkflowID(fmt.Sprintf("d4-%d-%s", i, run)))
		if err != nil {
			panic(err)
		}
		hs = append(hs, h5)
	}
	for _, h5 := range hs {
		_, _ = h5.GetResult()
	}
	st = "FAIL"
	if maxInFl.Load() <= 2 && maxInFl.Load() >= 1 {
		st = "PASS"
	}
	line(st, "D4 queue worker concurrency cap (limit 2)", fmt.Sprintf("max in flight observed=%d", maxInFl.Load()))
}

func own(exec string) {
	ctx := newCtx(exec)
	dbos.RegisterWorkflow(ctx, OrphanWF)
	if err := dbos.Launch(ctx); err != nil {
		panic(err)
	}
	// Idempotent: reuse the ID so a re-run does not create a second workflow.
	h, err := dbos.RunWorkflow(ctx, OrphanWF, "", dbos.WithWorkflowID("wf-orphan"))
	if err != nil {
		panic(err)
	}
	fmt.Println("started", h.GetWorkflowID(), "on executor", exec)
	time.Sleep(60 * time.Second)
}

func watch(exec string, secs int) {
	ctx := newCtx(exec)
	dbos.RegisterWorkflow(ctx, OrphanWF)
	if err := dbos.Launch(ctx); err != nil {
		panic(err)
	}
	defer dbos.Shutdown(ctx, time.Second)
	time.Sleep(time.Duration(secs) * time.Second)
	wfs, err := dbos.ListWorkflows(ctx, dbos.WithFilterWorkflowIDPrefix("wf-orphan"))
	if err != nil || len(wfs) == 0 {
		fmt.Println("watch: no workflow found", err)
		return
	}
	fmt.Printf("watch[%s]: after %ds wf-orphan status=%s owner-executor=%s\n", exec, secs, wfs[0].Status, wfs[0].ExecutorID)
}

func main() {
	switch os.Args[1] {
	case "basic":
		basic()
	case "own":
		own(os.Args[2])
	case "watch":
		n, _ := strconv.Atoi(os.Args[3])
		watch(os.Args[2], n)
	}
}
