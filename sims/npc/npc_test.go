package npc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/sims/cbs"
	"github.com/bill/ibps-bench/sims/simx"
)

const ourBank = "313000000000"

var mapper = mapping.SyntheticMapper{RejectCodes: map[string]string{}}

// fakeBank plays the payment service: acks with 202, then answers via the NPC callback like the real
// flow would (same outcome rules as the test-data conventions).
type fakeBank struct {
	npcURL    string
	delay     time.Duration
	capacity  chan struct{} // limits concurrent processing when set
	double    bool          // send two different receipts per payment
	seen      sync.Map
	deliv     atomic.Int32
	unique    atomic.Int32
	retryDown bool
}

func outcomeFor(p model.Payment) model.Outcome {
	now := time.Now()
	suffix := cbs.SplitSuffix(p.CreditorAcct)
	switch {
	case p.Currency != "CNY":
		return model.Outcome{Reason: model.ReasonCurrencyNotAllow, DecidedAt: now}
	case p.AmountFen > 100000000:
		return model.Outcome{Reason: model.ReasonAmountOverLimit, DecidedAt: now}
	case cbs.StatusForSuffix(suffix) == "":
		return model.Outcome{Reason: model.ReasonAccountNotFound, DecidedAt: now}
	case cbs.StatusForSuffix(suffix) == "CLOSED":
		return model.Outcome{Reason: model.ReasonAccountClosed, DecidedAt: now}
	case cbs.StatusForSuffix(suffix) == "FROZEN":
		return model.Outcome{Reason: model.ReasonAccountFrozen, DecidedAt: now}
	case cbs.StatusForSuffix(suffix) == "DORMANT":
		return model.Outcome{Reason: model.ReasonAccountDormant, DecidedAt: now}
	case strings.HasSuffix(p.CreditorName, " X"):
		return model.Outcome{Reason: model.ReasonNameMismatch, DecidedAt: now}
	case strings.HasPrefix(p.DebtorName, "BLOCKED PARTY"), strings.HasPrefix(p.DebtorName, "REVIEW-SLOW"):
		return model.Outcome{Reason: model.ReasonAMLHit, DecidedAt: now}
	}
	return model.Outcome{Accepted: true, DecidedAt: now}
}

func (f *fakeBank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	buf := new(bytes.Buffer)
	buf.ReadFrom(r.Body)
	p, err := mapper.InboundToPayment(buf.Bytes())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.deliv.Add(1)
	w.WriteHeader(http.StatusAccepted)
	if _, dup := f.seen.LoadOrStore(p.Key(), true); dup {
		return
	}
	f.unique.Add(1)
	go func() {
		if f.capacity != nil {
			f.capacity <- struct{}{}
			defer func() { <-f.capacity }()
		}
		time.Sleep(f.delay)
		raw, _, _ := mapper.OutboundReceipt(p, outcomeFor(p))
		f.send(raw)
		if f.double {
			// a second, different receipt for the same payment
			p2 := p
			p2.EndToEndID = p.EndToEndID // same original ids, different MsgId via a different posting ref
			p2.InstgAgent = "999999999999"
			raw2, _, _ := mapper.OutboundReceipt(p2, outcomeFor(p))
			raw2 = bytes.ReplaceAll(raw2, []byte("<OrgnlMsgId>"+p2.MsgID), []byte("<OrgnlMsgId>"+p.MsgID))
			f.send(raw2)
		}
	}()
}

func (f *fakeBank) send(raw []byte) {
	for i := 0; i < 200; i++ { // retry like step 6 does: same body, same MsgId
		resp, err := http.Post(f.npcURL+"/npc/ibps102", "application/xml", bytes.NewReader(raw))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type rig struct {
	npc   *Server
	npcH  *httptest.Server
	bank  *fakeBank
	bankH *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s := New(simx.New(0), mapper, 50000, ourBank, 100000000)
	mux := http.NewServeMux()
	s.Routes(mux)
	npcH := httptest.NewServer(mux)
	bank := &fakeBank{npcURL: npcH.URL, delay: 5 * time.Millisecond}
	bankH := httptest.NewServer(bank)
	t.Cleanup(func() { npcH.Close(); bankH.Close() })
	return &rig{s, npcH, bank, bankH}
}

func (r *rig) run(t *testing.T, req RunRequest) Summary {
	t.Helper()
	req.IngressURL = r.bankH.URL
	if req.RunID == "" {
		req.RunID = "run" + time.Now().Format("150405")
	}
	req.DrainS = 10
	b, _ := json.Marshal(req)
	resp, err := http.Post(r.npcH.URL+"/npc/run", "application/json", bytes.NewReader(b))
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("start run: %v %v", err, resp)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var st map[string]any
		resp, _ := http.Get(r.npcH.URL + "/npc/status")
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if st["state"] == "done" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, err = http.Get(r.npcH.URL + "/npc/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sum Summary
	if err := json.NewDecoder(resp.Body).Decode(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestRunProducesExpectedOutcomes(t *testing.T) {
	r := newRig(t)
	mix := DefaultMix(40, 30, 10, 10, 10) // heavy on rejects to exercise every category
	mix["currency_bad"], mix["amount_over"] = 5, 5
	sum := r.run(t, RunRequest{Rate: 150, DurationS: 2, WarmupS: 1, Mix: mix, Seed: 3})
	if sum.Sent < 400 || sum.Measured < 250 || sum.IngressErr != 0 || sum.Unanswered != 0 || sum.Received != sum.Measured {
		t.Fatalf("counts wrong: %+v", sum)
	}
	for name, cs := range sum.ByCategory {
		if cs.Mismatch != 0 {
			t.Errorf("category %s has %d unexpected outcomes: %v", name, cs.Mismatch, cs.Outcomes)
		}
	}
	for _, c := range Categories {
		if sum.ByCategory[c.Name] == nil || sum.ByCategory[c.Name].Sent == 0 {
			t.Errorf("category %s never generated", c.Name)
		}
	}
	if sum.Latency.P50 < 4 || sum.Latency.P50 > 200 || sum.Latency.Max < sum.Latency.P99 || sum.Latency.P99 < sum.Latency.P50 {
		t.Errorf("latency stats implausible: %+v", sum.Latency)
	}
	if sum.AckLatency.N == 0 || sum.AchievedTPS < 100 || sum.DoubleAnswered != 0 || sum.MultiReceipts != 0 {
		t.Errorf("summary: tps=%v ack=%+v double=%d multi=%d", sum.AchievedTPS, sum.AckLatency, sum.DoubleAnswered, sum.MultiReceipts)
	}
	if sum.Outcomes["ACCP"] == 0 || sum.Outcomes["AC01"] == 0 || len(sum.Timeline) == 0 || sum.GenLagMaxMs > 100 {
		t.Errorf("outcomes/timeline/lag: %v %d lag=%v", sum.Outcomes, len(sum.Timeline), sum.GenLagMaxMs)
	}
}

func TestDuplicateInjection(t *testing.T) {
	for _, mode := range []string{"dup_seq", "dup_conc"} {
		r := newRig(t)
		r.npc.Sim.Add(simx.Fault{Target: "send", Mode: mode, Rate: 0.3})
		sum := r.run(t, RunRequest{Rate: 100, DurationS: 2, Seed: 5, RunID: "dup" + mode[4:]})
		if sum.DupDeliveries == 0 || sum.DupBadAcks != 0 || sum.DoubleAnswered != 0 || sum.MultiReceipts != 0 {
			t.Errorf("%s: %+v", mode, sum)
		}
		if int(r.bank.unique.Load()) != sum.Sent || int(r.bank.deliv.Load()) != sum.Sent+sum.DupDeliveries {
			t.Errorf("%s: bank saw %d unique, %d deliveries; NPC sent %d + %d dups", mode, r.bank.unique.Load(), r.bank.deliv.Load(), sum.Sent, sum.DupDeliveries)
		}
	}
}

func TestDoubleAnswerIsDetected(t *testing.T) {
	r := newRig(t)
	r.bank.double = true
	sum := r.run(t, RunRequest{Rate: 50, DurationS: 1, Seed: 2})
	if sum.DoubleAnswered == 0 || sum.MultiReceipts == 0 {
		t.Errorf("two different receipt ids must be flagged: %+v", sum)
	}
}

func TestCallbackDownDelaysButDoesNotDouble(t *testing.T) {
	r := newRig(t)
	r.npc.Sim.Add(simx.Fault{Target: "ibps102", Mode: "down_503", DurationMs: 400})
	sum := r.run(t, RunRequest{Rate: 50, DurationS: 2, Seed: 4, StallMs: 300})
	if sum.DoubleAnswered != 0 || sum.MultiReceipts != 0 || sum.Unanswered != 0 || sum.StallCount == 0 || sum.StallSpanMs <= 0 {
		t.Errorf("callback outage: %+v", sum)
	}
	r2 := newRig(t)
	r2.npc.Sim.Add(simx.Fault{Target: "ibps102", Mode: "down_reset", DurationMs: 300})
	if s2 := r2.run(t, RunRequest{Rate: 30, DurationS: 1, Seed: 4}); s2.Unanswered != 0 || s2.DoubleAnswered != 0 {
		t.Errorf("reset outage: %+v", s2)
	}
	// ack_delay slows the 200 to the bank (receipt time is stamped on arrival, so latency is unchanged).
	r3 := newRig(t)
	r3.npc.Sim.Add(simx.Fault{Target: "ibps102", Mode: "ack_delay", LatencyMs: 40})
	raw, _, _ := mapper.OutboundReceipt(model.Payment{MsgID: "x-00000001", EndToEndID: "E", InstgAgent: "i"}, model.Outcome{Accepted: true, DecidedAt: time.Now()})
	start := time.Now()
	resp, err := http.Post(r3.npcH.URL+"/npc/ibps102", "application/xml", bytes.NewReader(raw))
	if err != nil || resp.StatusCode != 200 || time.Since(start) < 40*time.Millisecond {
		t.Errorf("ack delay: %v %v after %v", err, resp, time.Since(start))
	}
}

func TestStepUpFindsCeiling(t *testing.T) {
	r := newRig(t)
	r.bank.capacity = make(chan struct{}, 4) // 4 slots x 20 ms => about 200 TPS
	r.bank.delay = 20 * time.Millisecond
	sum := r.run(t, RunRequest{SLAms: 300, StepUp: &StepUp{StartRate: 50, Step: 50, StepDurS: 2, MaxRate: 800}, Seed: 1})
	if len(sum.Steps) < 3 || sum.CeilingRate < 100 || sum.CeilingRate > 250 || sum.CeilingCapped {
		t.Fatalf("ceiling %v capped=%v steps=%+v", sum.CeilingRate, sum.CeilingCapped, sum.Steps)
	}
	var breached bool
	for _, st := range sum.Steps {
		breached = breached || st.Breach
	}
	if !breached {
		t.Errorf("a step must have breached: %+v", sum.Steps)
	}
}

func TestStepUpCapped(t *testing.T) {
	r := newRig(t)
	sum := r.run(t, RunRequest{SLAms: 500, StepUp: &StepUp{StartRate: 20, Step: 20, StepDurS: 1, MaxRate: 60}, Seed: 1})
	if !sum.CeilingCapped || sum.CeilingRate != 60 {
		t.Errorf("no breach: expected capped ceiling 60, got %v capped=%v steps=%+v", sum.CeilingRate, sum.CeilingCapped, sum.Steps)
	}
}

func TestRunRequestHandling(t *testing.T) {
	r := newRig(t)
	post := func(body string) int {
		resp, err := http.Post(r.npcH.URL+"/npc/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, bad := range []string{`nope`, `{}`, `{"runId":"a","ingressUrl":"x","rate":0,"durationS":1}`,
		`{"runId":"a-b","ingressUrl":"x","rate":1,"durationS":1}`,
		`{"runId":"ab","ingressUrl":"x","rate":1,"durationS":1,"mix":{"bogus":1}}`} {
		if c := post(bad); c != 400 {
			t.Errorf("%s -> %d", bad, c)
		}
	}
	if c := post(`{"runId":"first","ingressUrl":"` + r.bankH.URL + `","rate":20,"durationS":2}`); c != 202 {
		t.Fatalf("start: %d", c)
	}
	if c := post(`{"runId":"second","ingressUrl":"` + r.bankH.URL + `","rate":20,"durationS":1}`); c != 409 {
		t.Errorf("second run while active: %d", c)
	}
	resp, _ := http.Get(r.npcH.URL + "/npc/results?detail=1")
	lines := new(bytes.Buffer)
	lines.ReadFrom(resp.Body)
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	http.Post(r.npcH.URL+"/npc/stop", "", nil)
	http.Post(r.npcH.URL+"/npc/reset", "", nil)
	if resp, _ := http.Get(r.npcH.URL + "/npc/results"); resp.StatusCode != 404 {
		t.Error("results after reset must be 404")
	}
	var st map[string]any
	resp, _ = http.Get(r.npcH.URL + "/npc/status")
	json.NewDecoder(resp.Body).Decode(&st)
	if st["state"] != "idle" {
		t.Errorf("status after reset: %v", st)
	}
	if c := post(`{"runId":"third","ingressUrl":"` + r.bankH.URL + `","rate":20,"durationS":1}`); c != 202 {
		t.Errorf("run after reset: %d", c)
	}
}

func TestDetailStream(t *testing.T) {
	r := newRig(t)
	r.run(t, RunRequest{Rate: 40, DurationS: 1, Seed: 9})
	resp, err := http.Get(r.npcH.URL + "/npc/results?detail=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	n := 0
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatal(err)
		}
		if rec["cat"] != "happy" || rec["ackCode"].(float64) != 202 || rec["recvNs"].(float64) <= 0 {
			t.Errorf("record %v", rec)
		}
		n++
	}
	if n < 35 {
		t.Errorf("only %d detail records", n)
	}
}

func TestReceiptEdgeCases(t *testing.T) {
	r := newRig(t)
	post := func(body string) int {
		resp, err := http.Post(r.npcH.URL+"/npc/ibps102", "application/xml", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("<garbage") != 400 {
		t.Error("garbage receipt must be 400")
	}
	p := model.Payment{MsgID: "old-00000001", EndToEndID: "E1", InstgAgent: "x"}
	raw, _, _ := mapper.OutboundReceipt(p, model.Outcome{Accepted: true, DecidedAt: time.Now()})
	if post(string(raw)) != 200 { // no run active: acknowledged and ignored
		t.Error("receipt with no run must still be acked")
	}
	r.run(t, RunRequest{Rate: 10, DurationS: 1, RunID: "cur"})
	if post(string(raw)) != 200 { // belongs to another run
		t.Error("receipt for another run must still be acked")
	}
	if post(strings.Replace(string(raw), "old-00000001", "cur-99999999", 1)) != 200 { // unknown seq
		t.Error("receipt for an unknown seq must still be acked")
	}
}

func TestHelpers(t *testing.T) {
	if id, n, ok := splitMsgID("run1-00000042"); !ok || id != "run1" || n != 42 {
		t.Error("splitMsgID")
	}
	if _, _, ok := splitMsgID("nodash"); ok {
		t.Error("splitMsgID without dash")
	}
	if _, _, ok := splitMsgID("a-b"); ok {
		t.Error("splitMsgID with non-numeric seq")
	}
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if percentile(v, 50) != 5 || percentile(v, 99) != 10 || percentile(v, 0) != 1 || percentile(nil, 50) != 0 || percentile(v, 100) != 10 {
		t.Error("percentile")
	}
	if st := stats(v); st.N != 10 || st.Max != 10 || st.MeanM != 5.5 || stats(nil).N != 0 {
		t.Errorf("stats %+v", st)
	}
	var total float64
	for _, x := range DefaultMix(94, 3, 1, 1, 1) {
		total += x
	}
	if total < 99.999 || total > 100.001 {
		t.Errorf("default mix sums to %v", total)
	}
	if catIndex("nope") != -1 || catIndex("aml_hit") < 0 {
		t.Error("catIndex")
	}
}

func TestBuildPaymentMatchesTestDataConventions(t *testing.T) {
	r := newRig(t)
	rs := &runState{req: RunRequest{RunID: "rid"}}
	rng := newTestRNG()
	for i, cat := range Categories {
		for k := 0; k < 30; k++ {
			p := r.npc.buildPayment(rs, i*100+k, catIndex(cat.Name), rng)
			suffix := cbs.SplitSuffix(p.CreditorAcct)
			bad := ""
			switch cat.Name {
			case "acct_not_found":
				if suffix < 9000 || suffix > 9099 {
					bad = "suffix"
				}
			case "acct_closed":
				if suffix < 9100 || suffix > 9199 {
					bad = "suffix"
				}
			case "acct_frozen":
				if suffix < 9200 || suffix > 9299 {
					bad = "suffix"
				}
			case "acct_dormant":
				if suffix < 9300 || suffix > 9399 {
					bad = "suffix"
				}
			case "name_mismatch":
				if !strings.HasSuffix(p.CreditorName, " X") || suffix > 8999 {
					bad = "name"
				}
			case "aml_hit":
				if !strings.HasPrefix(p.DebtorName, "BLOCKED PARTY ") {
					bad = "debtor"
				}
			case "review_fast":
				if !strings.HasPrefix(p.DebtorName, "REVIEW-FAST") {
					bad = "debtor"
				}
			case "review_slow":
				if !strings.HasPrefix(p.DebtorName, "REVIEW-SLOW") {
					bad = "debtor"
				}
			case "currency_bad":
				if p.Currency != "USD" {
					bad = "ccy"
				}
			case "amount_over":
				if p.AmountFen <= 100000000 {
					bad = "amount"
				}
			default: // happy
				if suffix > 8999 || p.CreditorName != cbs.AccountName(int(cbsIndex(p.CreditorAcct))) || p.Currency != "CNY" || p.AmountFen < 1 || p.AmountFen > 500000 {
					bad = "happy"
				}
			}
			if bad != "" {
				t.Fatalf("%s: bad %s in %+v", cat.Name, bad, p)
			}
			if p.MsgID != "rid-"+strings.TrimPrefix(p.MsgID, "rid-") || len(p.MsgID) > 35 || p.InstdAgent != ourBank {
				t.Fatalf("ids: %+v", p)
			}
			if _, err := mapper.BuildInbound(p); err != nil {
				t.Fatal(err)
			}
		}
	}
}
