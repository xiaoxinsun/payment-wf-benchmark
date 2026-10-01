package payment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

// These tests run the real DBOS engine against the dbos_test database on dbos-db (make up-v2; then
// `make test-db`). A separate database keeps them from cross-talking with the running payment replicas.
// They skip when it is not reachable.
var (
	testCtx dbos.Context
	skip    string
)

func TestMain(m *testing.M) {
	c, err := config.Load("../../deploy/config")
	if err != nil {
		panic(err)
	}
	fast := func(p config.RetryPolicy) config.RetryPolicy {
		p.Initial, p.MaxInterval = time.Millisecond, 4*time.Millisecond
		return p
	}
	c.Retries.Step3Validate, c.Retries.Step4Screen = fast(c.Retries.Step3Validate), fast(c.Retries.Step4Screen)
	c.Retries.Step5Post, c.Retries.Step6Respond = fast(c.Retries.Step5Post), fast(c.Retries.Step6Respond)
	Cfg = c
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName: "ibps-test", ApplicationVersion: "test-1", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DatabaseURL: "postgres://dbos:dbos@localhost:5433/dbos_test?sslmode=disable&connect_timeout=3", SystemDBStartupTimeout: 5 * time.Second})
	if err != nil {
		skip = err.Error()
	} else {
		Register(ctx)
		if err := dbos.Launch(ctx); err != nil {
			skip = err.Error()
		}
		testCtx = ctx
	}
	code := m.Run()
	if testCtx != nil && skip == "" {
		dbos.Shutdown(testCtx, 2*time.Second)
	}
	os.Exit(code)
}

type fakeCore struct {
	mu    sync.Mutex
	calls []string
	now   time.Time

	validate func() (steps.ValidateResult, error)
	screen   func() (steps.ScreenResult, error)
	review   func() (steps.ReviewResult, error)
	post     func() (steps.PostResult, error)
	respond  func(model.Outcome) error
}

func (f *fakeCore) add(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeCore) count(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == s {
			n++
		}
	}
	return n
}

func (f *fakeCore) ValidateAccount(context.Context, model.PaymentInput) (steps.ValidateResult, error) {
	f.add("validate")
	return f.validate()
}
func (f *fakeCore) ScreenAML(context.Context, model.PaymentInput) (steps.ScreenResult, error) {
	f.add("screen")
	return f.screen()
}
func (f *fakeCore) AwaitReview(context.Context, model.PaymentInput, string) (steps.ReviewResult, error) {
	f.add("review")
	return f.review()
}
func (f *fakeCore) PostSettlement(context.Context, model.PaymentInput) (steps.PostResult, error) {
	f.add("post")
	return f.post()
}
func (f *fakeCore) SendReceipt(_ context.Context, _ model.PaymentInput, o model.Outcome) error {
	if o.Accepted {
		f.add("respond:ACCP")
	} else {
		f.add("respond:" + o.Reason.ISOCode())
	}
	return f.respond(o)
}
func (f *fakeCore) RecordState(_ context.Context, _ model.PaymentInput, s model.PaymentState, _ model.RejectReason, _ time.Time) error {
	f.add("record:" + string(s))
	return nil
}

var seq int

func newFake(t *testing.T) (*fakeCore, model.PaymentInput) {
	t.Helper()
	if skip != "" {
		t.Skipf("dbos-db not reachable: %v", skip)
	}
	f := &fakeCore{now: time.Now()}
	f.validate = func() (steps.ValidateResult, error) { return steps.ValidateResult{DecidedAt: f.now}, nil }
	f.screen = func() (steps.ScreenResult, error) { return steps.ScreenResult{Verdict: "CLEAR", DecidedAt: f.now}, nil }
	f.post = func() (steps.PostResult, error) { return steps.PostResult{PostedAt: f.now}, nil }
	f.respond = func(model.Outcome) error { return nil }
	Impl = f
	seq++
	p := model.Payment{MsgID: fmt.Sprintf("T%d-%d", time.Now().UnixNano(), seq), EndToEndID: "E1", InstgAgent: "102100099996",
		InstdAgent: Cfg.SLA.OurBankCode, AmountFen: 1000, Currency: "CNY", CreditorAcct: "6222000000000042", CreditorName: "C", DebtorName: "D"}
	return f, model.PaymentInput{Payment: p, ReceivedAt: f.now, Deadline: f.now.Add(Cfg.SLA.Budget)}
}

func exec(t *testing.T, in model.PaymentInput) model.Outcome {
	t.Helper()
	h, err := dbos.RunWorkflow(testCtx, Workflow, in, dbos.WithWorkflowID(in.Payment.WorkflowID()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.GetResult()
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	return out
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
}

func TestHappyPath(t *testing.T) {
	f, in := newFake(t)
	if out := exec(t, in); !out.Accepted {
		t.Fatalf("%+v", out)
	}
	assertOrder(t, f.calls, "validate", "record:VALIDATED", "screen", "record:SCREENED", "post",
		"record:SETTLED", "respond:ACCP", "record:ACCEPTED_SENT")
}

func TestStep2RejectsMakeNoExternalCall(t *testing.T) {
	cases := map[string]func(*model.Payment){
		"AM03": func(p *model.Payment) { p.Currency = "USD" },
		"AM02": func(p *model.Payment) { p.AmountFen = Cfg.SLA.AmountLimitFen + 1 },
		"AGNT": func(p *model.Payment) { p.InstdAgent = "000000000000" },
	}
	for code, edit := range cases {
		f, in := newFake(t)
		edit(&in.Payment)
		out := exec(t, in)
		if out.Accepted || out.Reason.ISOCode() != code {
			t.Fatalf("%s: %+v", code, out)
		}
		assertOrder(t, f.calls, "record:REJECTED", "respond:"+code, "record:REJECTED_SENT")
	}
}

func TestValidateAndAMLRejectsNeverPost(t *testing.T) {
	f, in := newFake(t)
	f.validate = func() (steps.ValidateResult, error) {
		return steps.ValidateResult{Reject: model.ReasonAccountClosed, DecidedAt: time.Now()}, nil
	}
	if out := exec(t, in); out.Reason != model.ReasonAccountClosed {
		t.Fatalf("%+v", out)
	}
	assertOrder(t, f.calls, "validate", "record:REJECTED", "respond:AC04", "record:REJECTED_SENT")

	f, in = newFake(t)
	f.screen = func() (steps.ScreenResult, error) {
		return steps.ScreenResult{Verdict: "REJECT", Reject: model.ReasonAMLHit, DecidedAt: time.Now()}, nil
	}
	if out := exec(t, in); out.Reason != model.ReasonAMLHit || f.count("post") != 0 {
		t.Fatalf("%+v %v", out, f.calls)
	}
}

func TestReviewPaths(t *testing.T) {
	reviewing := func(f *fakeCore, res steps.ReviewResult, err error) {
		f.screen = func() (steps.ScreenResult, error) {
			return steps.ScreenResult{Verdict: "REVIEW", CaseID: "C1", DecidedAt: time.Now()}, nil
		}
		f.review = func() (steps.ReviewResult, error) { return res, err }
	}
	f, in := newFake(t)
	reviewing(f, steps.ReviewResult{DecidedAt: time.Now()}, nil)
	if out := exec(t, in); !out.Accepted || f.count("post") != 1 || f.count("review") != 1 {
		t.Fatalf("cleared: %+v %v", out, f.calls)
	}
	f, in = newFake(t)
	reviewing(f, steps.ReviewResult{Reject: model.ReasonAMLReviewTimeout, DecidedAt: time.Now()}, nil)
	if out := exec(t, in); out.Reason.ISOCode() != "RR04" || f.count("post") != 0 {
		t.Fatalf("timeout: %+v %v", out, f.calls)
	}
	f, in = newFake(t)
	reviewing(f, steps.ReviewResult{}, errors.New("boom"))
	if out := exec(t, in); out.Reason != model.ReasonAMLReviewTimeout || f.count("post") != 0 {
		t.Fatalf("failure: %+v %v", out, f.calls)
	}
}

func TestRetriesExhaustedInsideSLARejectsAB05(t *testing.T) {
	f, in := newFake(t)
	f.validate = func() (steps.ValidateResult, error) { return steps.ValidateResult{}, errors.New("cbs 503") }
	out := exec(t, in)
	if out.Reason != model.ReasonSLATimeout || f.count("post") != 0 {
		t.Fatalf("%+v %v", out, f.calls)
	}
	if n := f.count("validate"); n != Cfg.Retries.Step3Validate.MaxAttempts {
		t.Errorf("validate attempts %d, want %d", n, Cfg.Retries.Step3Validate.MaxAttempts)
	}
	f, in = newFake(t)
	f.screen = func() (steps.ScreenResult, error) { return steps.ScreenResult{}, errors.New("aml down") }
	if out := exec(t, in); out.Reason != model.ReasonSLATimeout || f.count("post") != 0 {
		t.Fatalf("aml: %+v %v", out, f.calls)
	}
}

func TestSettlementAndReceiptRetryUntilSuccess(t *testing.T) {
	f, in := newFake(t)
	f.post = func() (steps.PostResult, error) {
		if f.count("post") <= 6 {
			return steps.PostResult{}, errors.New("cbs timeout")
		}
		return steps.PostResult{PostedAt: time.Now()}, nil
	}
	f.respond = func(model.Outcome) error {
		if f.count("respond:ACCP") <= 5 {
			return errors.New("npc down")
		}
		return nil
	}
	out := exec(t, in)
	if !out.Accepted || f.count("post") != 7 || f.count("respond:ACCP") != 6 {
		t.Fatalf("%+v posts=%d sends=%d", out, f.count("post"), f.count("respond:ACCP"))
	}
}

func TestPostAfterSLAExpiredStillSettles(t *testing.T) {
	f, in := newFake(t)
	in.Deadline = in.ReceivedAt.Add(-time.Hour)
	if out := exec(t, in); !out.Accepted || f.count("post") != 1 {
		t.Fatalf("%+v %v", out, f.calls)
	}
}

func TestWorkflowIDDedup(t *testing.T) {
	f, in := newFake(t)
	first := exec(t, in)
	second := exec(t, in) // same workflow ID: attaches to the finished workflow, runs nothing
	if !first.Accepted || !second.Accepted || f.count("post") != 1 || f.count("respond:ACCP") != 1 {
		t.Fatalf("dedup: %+v %+v %v", first, second, f.calls)
	}
	// concurrent duplicates
	f, in = newFake(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := dbos.RunWorkflow(testCtx, Workflow, in, dbos.WithWorkflowID(in.Payment.WorkflowID()))
			if err == nil {
				h.GetResult()
			}
		}()
	}
	wg.Wait()
	if f.count("post") != 1 || f.count("respond:ACCP") != 1 {
		t.Fatalf("8 concurrent starts must run once: %v", f.calls)
	}
}

func TestStarterCapAndAwait(t *testing.T) {
	f, in := newFake(t)
	block := make(chan struct{})
	f.post = func() (steps.PostResult, error) { <-block; return steps.PostResult{PostedAt: time.Now()}, nil }
	s := NewStarter(testCtx, 1, 100*time.Millisecond)
	if err := s.Start(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	_, in2 := newFake(t)
	Impl = f
	if err := s.Start(context.Background(), in2); err == nil {
		t.Error("second start must hit the in-flight cap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx, in2); err == nil {
		t.Error("cancelled context must fail")
	}
	close(block)
	out, err := s.Await(context.Background(), in)
	if err != nil || !out.Accepted {
		t.Fatalf("await: %+v %v", out, err)
	}
	if _, err := s.Await(context.Background(), in2); err == nil {
		t.Error("await of an unknown workflow must fail")
	}
	time.Sleep(50 * time.Millisecond)
	if err := s.Start(context.Background(), in2); err != nil { // slot released
		t.Errorf("slot must be released after the workflow finishes: %v", err)
	}
}
