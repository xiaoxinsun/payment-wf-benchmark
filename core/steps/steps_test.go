package steps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/model"
)

func testCfg() config.Config {
	c, err := config.Load("../../deploy/config")
	if err != nil {
		panic(err)
	}
	c.Timeouts.Step3Validate = 100 * time.Millisecond
	c.Timeouts.Step4Screen = 100 * time.Millisecond
	c.Timeouts.Step4bReviewPoll = 100 * time.Millisecond
	c.Timeouts.Step5Post = 100 * time.Millisecond
	c.Timeouts.Step5StatusQuery = 100 * time.Millisecond
	c.Timeouts.Step6Respond = 100 * time.Millisecond
	c.Retries.Step4bReview.PollEvery = 10 * time.Millisecond
	return c
}

func testPayment() model.Payment {
	return model.Payment{MsgID: "M1", EndToEndID: "E1", TxID: "T1", InstgAgent: "102100099996", InstdAgent: "313000000000",
		AmountFen: 1000, Currency: "CNY", DebtorName: "D", CreditorName: "C", CreditorAcct: "6222000000000042",
		CreDtTm: time.Now()}
}

func input(deadline time.Duration) model.PaymentInput {
	return model.PaymentInput{Payment: testPayment(), ReceivedAt: time.Now(), Deadline: time.Now().Add(deadline)}
}

func newDeps(cbs, aml, npc string) *Deps {
	return &Deps{Cfg: testCfg(), EP: config.Endpoints{CBSURL: cbs, AMLURL: aml, NPCURL: npc},
		HTTP: NewHTTPClient(), Mapper: mapping.SyntheticMapper{}}
}

func TestCheck(t *testing.T) {
	sla := testCfg().SLA
	base := testPayment()
	base.InstdAgent = sla.OurBankCode
	cases := []struct {
		name string
		edit func(*model.Payment)
		want model.RejectReason
	}{
		{"ok", func(p *model.Payment) {}, ""},
		{"wrong receiver", func(p *model.Payment) { p.InstdAgent = "999" }, model.ReasonWrongReceiver},
		{"currency", func(p *model.Payment) { p.Currency = "USD" }, model.ReasonCurrencyNotAllow},
		{"zero amount", func(p *model.Payment) { p.AmountFen = 0 }, model.ReasonFormatInvalid},
		{"over limit", func(p *model.Payment) { p.AmountFen = sla.AmountLimitFen + 1 }, model.ReasonAmountOverLimit},
		{"at limit", func(p *model.Payment) { p.AmountFen = sla.AmountLimitFen }, ""},
	}
	for _, c := range cases {
		p := base
		c.edit(&p)
		got, rejected := Check(sla, p)
		if got != c.want || rejected != (c.want != "") {
			t.Errorf("%s: got %q,%v want %q", c.name, got, rejected, c.want)
		}
	}
}

func TestValidateAccount(t *testing.T) {
	var status atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/accounts/6222000000000042/validate") || r.URL.Query().Get("ccy") != "CNY" || r.URL.Query().Get("name") != "C" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	d := newDeps(srv.URL, "", "")
	for reason, want := range validateReasons {
		status.Store(422)
		body.Store(`{"reason":"` + reason + `"}`)
		res, err := d.ValidateAccount(context.Background(), input(time.Minute))
		if err != nil || res.Reject != want {
			t.Errorf("%s: %+v %v", reason, res, err)
		}
	}
	status.Store(200)
	body.Store(`{}`)
	if res, err := d.ValidateAccount(context.Background(), input(time.Minute)); err != nil || res.Reject != "" {
		t.Errorf("valid: %+v %v", res, err)
	}
	status.Store(422)
	body.Store(`{"reason":"WAT"}`)
	if _, err := d.ValidateAccount(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("unknown 422 reason must be transient: %v", err)
	}
	status.Store(503)
	if _, err := d.ValidateAccount(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("503 must be transient: %v", err)
	}
	// SLA already spent: reject without any call.
	res, err := d.ValidateAccount(context.Background(), input(-time.Second))
	if err != nil || res.Reject != model.ReasonSLATimeout {
		t.Errorf("expired SLA: %+v %v", res, err)
	}
	// Connection failure is transient.
	dead := newDeps("http://127.0.0.1:1", "", "")
	if _, err := dead.ValidateAccount(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("refused connection must be transient: %v", err)
	}
}

func TestScreenAML(t *testing.T) {
	var resp atomic.Value
	var code atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["debtorName"] != "D" || in["creditorName"] != "C" {
			t.Errorf("bad screen body %v", in)
		}
		w.WriteHeader(int(code.Load()))
		w.Write([]byte(resp.Load().(string)))
	}))
	defer srv.Close()
	d := newDeps("", srv.URL, "")
	code.Store(200)
	for body, want := range map[string]ScreenResult{
		`{"result":"CLEAR"}`:                {Verdict: "CLEAR"},
		`{"result":"HIT","listId":"L1"}`:    {Verdict: "REJECT", Reject: model.ReasonAMLHit},
		`{"result":"REVIEW","caseId":"C9"}`: {Verdict: "REVIEW", CaseID: "C9"},
	} {
		resp.Store(body)
		got, err := d.ScreenAML(context.Background(), input(time.Minute))
		if err != nil || got.Verdict != want.Verdict || got.Reject != want.Reject || got.CaseID != want.CaseID {
			t.Errorf("%s: %+v %v", body, got, err)
		}
	}
	resp.Store(`{"result":"CLEAR"}`)
	if res, _ := d.ScreenAML(context.Background(), input(-time.Second)); res.Reject != model.ReasonSLATimeout {
		t.Errorf("expired SLA before screening: %+v", res)
	}
	resp.Store(`{"result":"WAT"}`)
	if _, err := d.ScreenAML(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("unknown verdict must be transient: %v", err)
	}
	resp.Store(`not json`)
	if _, err := d.ScreenAML(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("bad body must be transient: %v", err)
	}
	code.Store(500)
	if _, err := d.ScreenAML(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("500 must be transient: %v", err)
	}
	if _, err := newDeps("", "http://127.0.0.1:1", "").ScreenAML(context.Background(), input(time.Minute)); !errors.Is(err, ErrTransient) {
		t.Errorf("refused must be transient: %v", err)
	}
	// SLA runs out while the (slow) AML answers CLEAR: rejected at the last decision point.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Write([]byte(`{"result":"CLEAR"}`))
	}))
	defer slow.Close()
	res, err := newDeps("", slow.URL, "").ScreenAML(context.Background(), input(30*time.Millisecond))
	if err != nil && !errors.Is(err, ErrTransient) {
		t.Fatal(err)
	}
	if err == nil && res.Reject != model.ReasonSLATimeout {
		t.Errorf("late CLEAR must reject with SLA_TIMEOUT: %+v", res)
	}
}

func TestAwaitReview(t *testing.T) {
	var polls atomic.Int32
	var final atomic.Value
	final.Store("CLEARED")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		switch {
		case n == 1:
			w.WriteHeader(500) // poll error is treated as still open
		case n == 2:
			w.Write([]byte(`{"state":"OPEN"}`))
		case n == 3:
			w.Write([]byte(`garbage`))
		default:
			w.Write([]byte(`{"state":"` + final.Load().(string) + `"}`))
		}
	}))
	defer srv.Close()
	d := newDeps("", srv.URL, "")
	res, err := d.AwaitReview(context.Background(), input(time.Minute), "C1")
	if err != nil || res.Reject != "" || polls.Load() < 4 {
		t.Errorf("cleared: %+v %v polls=%d", res, err, polls.Load())
	}
	polls.Store(0)
	final.Store("CONFIRMED_HIT")
	if res, _ := d.AwaitReview(context.Background(), input(time.Minute), "C1"); res.Reject != model.ReasonAMLHit {
		t.Errorf("confirmed hit: %+v", res)
	}
	// Never resolves: rejected at the SLA deadline with RR04.
	polls.Store(0)
	final.Store("OPEN")
	start := time.Now()
	res, err = d.AwaitReview(context.Background(), input(120*time.Millisecond), "C1")
	if err != nil || res.Reject != model.ReasonAMLReviewTimeout || time.Since(start) > time.Second {
		t.Errorf("timeout: %+v %v after %v", res, err, time.Since(start))
	}
	// Cleared but the SLA ran out in the meantime.
	final.Store("CLEARED")
	polls.Store(10)
	res, _ = d.AwaitReview(context.Background(), input(-time.Second), "C1")
	if res.Reject != model.ReasonAMLReviewTimeout {
		t.Errorf("expired before first poll: %+v", res)
	}
	// Cancelled context is a transient error, not a business outcome.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	final.Store("OPEN")
	if _, err := d.AwaitReview(ctx, input(time.Minute), "C1"); !errors.Is(err, ErrTransient) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestSettlementAccountSpread(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		seen[SettlementAccount(model.Payment{MsgID: string(rune('a' + i%26)), EndToEndID: time.Duration(i).String(), InstgAgent: "x"}.PostingRef(), 16)]++
	}
	if len(seen) != 16 {
		t.Errorf("expected all 16 shards used, got %d", len(seen))
	}
	if SettlementAccount("ref", 1) != "SETTLE-IBPS-0" {
		t.Error("one shard must always be shard 0")
	}
	a, b := SettlementAccount("ref", 16), SettlementAccount("ref", 16)
	if a != b {
		t.Error("shard must be deterministic")
	}
}

// fakeCBS models the CBS posting semantics the resolver depends on.
type fakeCBS struct {
	posts       atomic.Int32
	stored      atomic.Bool
	postMode    string // ok | timeout | drop | 500 | 409
	statusFails bool
}

func (f *fakeCBS) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/postings":
			f.posts.Add(1)
			var b postingBody
			json.NewDecoder(r.Body).Decode(&b)
			if b.PostingRef != "IBPS-102100099996-M1-E1" || len(b.Legs) != 2 || b.Legs[0].Side != "D" || b.Legs[1].Side != "C" ||
				b.Legs[0].AmountFen != b.Legs[1].AmountFen || b.Legs[1].Account != "6222000000000042" || !strings.HasPrefix(b.Legs[0].Account, "SETTLE-IBPS-") {
				t.Errorf("bad posting body %+v", b)
			}
			switch f.postMode {
			case "ok":
				if f.stored.Swap(true) {
					w.WriteHeader(200)
				} else {
					w.WriteHeader(201)
				}
				w.Write([]byte(`{"status":"POSTED","postedAt":"2026-09-28T10:00:00Z"}`))
			case "timeout": // never committed
				time.Sleep(300 * time.Millisecond)
			case "drop": // committed, response lost
				f.stored.Store(true)
				time.Sleep(300 * time.Millisecond)
			case "500":
				w.WriteHeader(500)
			case "409":
				w.WriteHeader(409)
				w.Write([]byte("unbalanced"))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/postings/"):
			if f.statusFails {
				w.WriteHeader(500)
				return
			}
			if f.stored.Load() {
				w.Write([]byte(`{"status":"POSTED","postedAt":"2026-09-28T10:00:00Z"}`))
				return
			}
			w.WriteHeader(404)
		}
	})
}

func TestPostSettlement(t *testing.T) {
	f := &fakeCBS{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	d := newDeps(srv.URL, "", "")
	in := input(time.Minute)

	f.postMode = "ok"
	res, err := d.PostSettlement(context.Background(), in)
	if err != nil || res.AlreadyPosted || res.PostedAt.IsZero() {
		t.Fatalf("first post: %+v %v", res, err)
	}
	if res, err = d.PostSettlement(context.Background(), in); err != nil || !res.AlreadyPosted {
		t.Fatalf("second post must report ALREADY_POSTED: %+v %v", res, err)
	}

	// I4: committed, response dropped -> status query finds it, no error.
	f2 := &fakeCBS{postMode: "drop"}
	srv2 := httptest.NewServer(f2.handler(t))
	defer srv2.Close()
	res, err = newDeps(srv2.URL, "", "").PostSettlement(context.Background(), in)
	if err != nil || !res.AlreadyPosted {
		t.Errorf("I4 dropped response: %+v %v", res, err)
	}

	// I5: timed out before commit -> NOT_FOUND -> transient error so the engine retries the same ref.
	f3 := &fakeCBS{postMode: "timeout"}
	srv3 := httptest.NewServer(f3.handler(t))
	defer srv3.Close()
	if _, err = newDeps(srv3.URL, "", "").PostSettlement(context.Background(), in); !errors.Is(err, ErrTransient) {
		t.Errorf("I5 must be transient: %v", err)
	}
	// ... and after the retry the post lands exactly once.
	f3.postMode = "ok"
	if res, err = newDeps(srv3.URL, "", "").PostSettlement(context.Background(), in); err != nil || res.AlreadyPosted {
		t.Errorf("I5 retry: %+v %v", res, err)
	}

	// 5xx and a failing status query are both transient.
	f4 := &fakeCBS{postMode: "500", statusFails: true}
	srv4 := httptest.NewServer(f4.handler(t))
	defer srv4.Close()
	if _, err = newDeps(srv4.URL, "", "").PostSettlement(context.Background(), in); !errors.Is(err, ErrTransient) {
		t.Errorf("500 + failing status query must be transient: %v", err)
	}
	f4.statusFails = false
	f4.stored.Store(true)
	if res, err = newDeps(srv4.URL, "", "").PostSettlement(context.Background(), in); err != nil || !res.AlreadyPosted {
		t.Errorf("500 but already posted: %+v %v", res, err)
	}

	// 409 is the bug guard: a hard, non-transient error.
	f5 := &fakeCBS{postMode: "409"}
	srv5 := httptest.NewServer(f5.handler(t))
	defer srv5.Close()
	if _, err = newDeps(srv5.URL, "", "").PostSettlement(context.Background(), in); err == nil || errors.Is(err, ErrTransient) {
		t.Errorf("409 must be a hard error: %v", err)
	}

	// Connection refused: both the post and the status query fail.
	if _, err = newDeps("http://127.0.0.1:1", "", "").PostSettlement(context.Background(), in); !errors.Is(err, ErrTransient) {
		t.Errorf("refused: %v", err)
	}
	// Unexpected status query code.
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(418)
	}))
	defer odd.Close()
	if _, err = newDeps(odd.URL, "", "").PostSettlement(context.Background(), in); !errors.Is(err, ErrTransient) {
		t.Errorf("odd status: %v", err)
	}
}

func TestSendReceipt(t *testing.T) {
	var got atomic.Value
	var code atomic.Int32
	code.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		got.Store(string(b[:n]))
		w.WriteHeader(int(code.Load()))
	}))
	defer srv.Close()
	d := newDeps("", "", srv.URL)
	in := input(time.Minute)
	if err := d.SendReceipt(context.Background(), in, model.Outcome{Accepted: true, DecidedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first := got.Load().(string)
	if !strings.Contains(first, "<PrcSts>accepted</PrcSts>") || !strings.Contains(first, mapping.ReceiptMsgID(in.Payment)) {
		t.Errorf("unexpected receipt %s", first)
	}
	code.Store(503)
	if err := d.SendReceipt(context.Background(), in, model.Outcome{Accepted: true, DecidedAt: time.Now()}); !errors.Is(err, ErrTransient) {
		t.Errorf("503 must be transient: %v", err)
	}
	if err := newDeps("", "", "http://127.0.0.1:1").SendReceipt(context.Background(), in, model.Outcome{Accepted: true, DecidedAt: time.Now()}); !errors.Is(err, ErrTransient) {
		t.Errorf("refused must be transient: %v", err)
	}
	// A reject with an unknown reason is a programming error and must not be retried forever.
	if err := d.SendReceipt(context.Background(), in, model.Outcome{Reason: "BOGUS"}); err == nil || errors.Is(err, ErrTransient) {
		t.Errorf("bogus reason: %v", err)
	}
}
