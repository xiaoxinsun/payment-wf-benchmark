// Package npc simulates the IBPS NPC: an open-model ibps.101 load generator plus the ibps.102 receiver.
// Send, ack and receipt timestamps are all taken on this process's clock, so latency needs no clock sync.
package npc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/sims/cbs"
	"github.com/bill/ibps-bench/sims/simx"
)

// Category is a kind of test payment with a known, deterministic expected outcome.
type Category struct {
	Name   string
	Expect string // "ACCP" or the ISO reject code
}

var Categories = []Category{
	{"happy", "ACCP"}, {"acct_not_found", "AC01"}, {"acct_closed", "AC04"}, {"acct_frozen", "AC06"},
	{"acct_dormant", "AC06"}, {"name_mismatch", "BE01"}, {"aml_hit", "RR04"}, {"review_fast", "ACCP"},
	{"review_slow", "RR04"}, {"currency_bad", "AM03"}, {"amount_over", "AM02"},
}

func catIndex(name string) int {
	for i, c := range Categories {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// DefaultMix expands the plan's default mix (94/3/1/1/1) into categories; the 3% account rejects are
// spread evenly over the five account-level rejects.
func DefaultMix(happy, acctReject, amlHit, reviewFast, reviewSlow float64) map[string]float64 {
	each := acctReject / 5
	return map[string]float64{"happy": happy, "acct_not_found": each, "acct_closed": each, "acct_frozen": each,
		"acct_dormant": each, "name_mismatch": each, "aml_hit": amlHit, "review_fast": reviewFast, "review_slow": reviewSlow}
}

type StepUp struct {
	StartRate float64 `json:"startRate"`
	Step      float64 `json:"step"`
	StepDurS  int     `json:"stepDurS"`
	MaxRate   float64 `json:"maxRate"`
}

type RunRequest struct {
	RunID      string             `json:"runId"`
	Rate       float64            `json:"rate"`
	DurationS  int                `json:"durationS"`
	WarmupS    int                `json:"warmupS"`
	DrainS     int                `json:"drainS"`
	Mode       string             `json:"mode"` // async (default) | sync
	IngressURL string             `json:"ingressUrl"`
	Mix        map[string]float64 `json:"mix"`
	Seed       uint64             `json:"seed"`
	StepUp     *StepUp            `json:"stepUp,omitempty"`
	SLAms      int                `json:"slaMs"`
	StallMs    int                `json:"stallMs"`
}

type recData struct {
	cat       uint8
	step      int16
	sched     int64 // ns since run start
	send      int64
	ack       int64
	recv      int64
	ackCode   int32
	sent      bool
	firstID   string
	nReceipts int32
	distinct  int32 // receipts whose MsgId differs from the first: a double answer
	accepted  bool
	iso       string
	dupSent   int32
	dupBad    int32
}

type record struct {
	mu sync.Mutex
	recData
}

// Server is the NPC simulator.
type Server struct {
	Sim         *simx.Sim
	Mapper      mapping.MessageMapper
	Accounts    int
	OurBank     string
	AmountLimit int64
	HTTP        *http.Client

	mu     sync.RWMutex
	run    *runState
	callbk atomic.Int64
}

type runState struct {
	req      RunRequest
	t0       time.Time
	recs     []*record
	state    atomic.Value // running | draining | done
	stop     atomic.Bool
	genLagNs atomic.Int64
	steps    []StepStat
	stepsMu  sync.Mutex
	ceiling  float64
	capped   bool
	breached bool
	cancel   context.CancelFunc
}

func New(sim *simx.Sim, m mapping.MessageMapper, accounts int, ourBank string, amountLimit int64) *Server {
	return &Server{Sim: sim, Mapper: m, Accounts: accounts, OurBank: ourBank, AmountLimit: amountLimit,
		HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			MaxIdleConns: 2048, MaxIdleConnsPerHost: 1024, IdleConnTimeout: 90 * time.Second}}}
}

func (s *Server) Routes(mux *http.ServeMux) {
	s.Sim.Control(mux)
	mux.HandleFunc("POST /npc/ibps102", s.receipt)
	mux.HandleFunc("POST /npc/run", s.startRun)
	mux.HandleFunc("POST /npc/stop", func(w http.ResponseWriter, _ *http.Request) {
		if r := s.current(); r != nil {
			r.stop.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /npc/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		if s.run != nil {
			s.run.stop.Store(true)
			s.run.cancel()
		}
		s.run = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /npc/status", s.status)
	mux.HandleFunc("GET /npc/results", s.results)
}

func (s *Server) current() *runState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.run
}

// ---- receiving ibps.102 ----------------------------------------------------------------------------

func (s *Server) receipt(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	s.Sim.Sleep()
	if f := s.Sim.Pick("ibps102"); f != nil {
		switch f.Mode {
		case "down_503":
			http.Error(w, "callback down", http.StatusServiceUnavailable)
			return
		case "down_reset":
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		case "ack_delay":
			time.Sleep(time.Duration(f.LatencyMs) * time.Millisecond)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	rc, err := s.Mapper.ParseReceipt(raw)
	if err != nil {
		http.Error(w, "bad receipt", http.StatusBadRequest)
		return
	}
	s.callbk.Add(1)
	run := s.current()
	if run == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	rid, seq, ok := splitMsgID(rc.OrigMsgID)
	if ok && rid == run.req.RunID {
		if rec := run.rec(seq); rec != nil {
			rec.mu.Lock()
			rec.nReceipts++
			if rec.firstID == "" {
				rec.firstID = rc.MsgID
				rec.recv = int64(now.Sub(run.t0))
				rec.accepted = rc.Accepted
				rec.iso = rc.IsoReason
			} else if rec.firstID != rc.MsgID {
				rec.distinct++
			}
			rec.mu.Unlock()
		}
	}
	w.WriteHeader(http.StatusOK)
}

func splitMsgID(id string) (runID string, seq int, ok bool) {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+1:])
	return id[:i], n, err == nil
}

func (rs *runState) rec(seq int) *record {
	if seq < 0 {
		return nil
	}
	rs.stepsMu.Lock() // guards recs growth as well as steps
	defer rs.stepsMu.Unlock()
	if seq >= len(rs.recs) {
		return nil
	}
	return rs.recs[seq]
}

// ---- generating ibps.101 ---------------------------------------------------------------------------

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunID == "" || req.IngressURL == "" ||
		(req.StepUp == nil && (req.Rate <= 0 || req.DurationS <= 0)) {
		http.Error(w, "need runId, ingressUrl and rate+durationS (or stepUp)", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.RunID, "-") {
		http.Error(w, "runId must not contain '-'", http.StatusBadRequest)
		return
	}
	if len(req.Mix) == 0 {
		req.Mix = map[string]float64{"happy": 100}
	}
	for k := range req.Mix {
		if catIndex(k) < 0 {
			http.Error(w, "unknown category "+k, http.StatusBadRequest)
			return
		}
	}
	if req.DrainS == 0 {
		req.DrainS = 60
	}
	if req.SLAms == 0 {
		req.SLAms = 5000
	}
	if req.StallMs == 0 {
		req.StallMs = 2000
	}
	if req.Seed == 0 {
		req.Seed = 1
	}
	s.mu.Lock()
	if s.run != nil {
		if st, _ := s.run.state.Load().(string); st != "done" {
			s.mu.Unlock()
			http.Error(w, "a run is already active", http.StatusConflict)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	rs := &runState{req: req, cancel: cancel}
	rs.state.Store("running")
	s.run = rs
	s.mu.Unlock()
	go s.generate(ctx, rs)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) pickCategory(mix map[string]float64, rng *rand.Rand) int {
	var total float64
	names := make([]string, 0, len(mix))
	for k, v := range mix {
		total += v
		names = append(names, k)
	}
	sort.Strings(names) // deterministic iteration
	x := rng.Float64() * total
	for _, k := range names {
		if x < mix[k] {
			return catIndex(k)
		}
		x -= mix[k]
	}
	return catIndex(names[len(names)-1])
}

func (s *Server) buildPayment(rs *runState, seq, cat int, rng *rand.Rand) model.Payment {
	pick := func(lo, hi int) int { // account index with last-four-digits suffix in [lo,hi]
		blocks := s.Accounts / 10000
		if blocks < 1 {
			blocks = 1
		}
		return rng.IntN(blocks)*10000 + lo + rng.IntN(hi-lo+1)
	}
	idx := pick(0, 8999)
	credName := cbs.AccountName(idx)
	id := fmt.Sprintf("%s-%08d", rs.req.RunID, seq)
	p := model.Payment{
		MsgID: id, EndToEndID: "E" + id, TxID: "T" + id, CreDtTm: time.Now(),
		InstgAgent: "102100099996", InstdAgent: s.OurBank, AmountFen: int64(1 + rng.IntN(500000)), Currency: "CNY",
		SettlementDate: time.Now().In(mapping.CST).Format("2006-01-02"), BizTp: "A100", BizKind: "02102",
		DebtorName: fmt.Sprintf("PAYER %06d", rng.IntN(1000000)), DebtorAcct: "6217000000000001", DebtorBank: "102100099996",
		CreditorBank: s.OurBank, Remark: "benchmark " + id,
	}
	switch Categories[cat].Name {
	case "acct_not_found":
		idx = pick(9000, 9099)
	case "acct_closed":
		idx = pick(9100, 9199)
	case "acct_frozen":
		idx = pick(9200, 9299)
	case "acct_dormant":
		idx = pick(9300, 9399)
	case "name_mismatch":
		credName += " X"
	case "aml_hit":
		p.DebtorName = fmt.Sprintf("BLOCKED PARTY %02d", 1+rng.IntN(20))
	case "review_fast":
		p.DebtorName = fmt.Sprintf("REVIEW-FAST %06d", rng.IntN(1000000))
	case "review_slow":
		p.DebtorName = fmt.Sprintf("REVIEW-SLOW %06d", rng.IntN(1000000))
	case "currency_bad":
		p.Currency = "USD"
	case "amount_over":
		p.AmountFen = s.AmountLimit + 1 + int64(rng.IntN(1000))
	}
	p.CreditorAcct = cbs.AccountID(idx)
	if Categories[cat].Name != "name_mismatch" {
		credName = cbs.AccountName(idx)
	}
	p.CreditorName = credName
	return p
}

func (s *Server) generate(ctx context.Context, rs *runState) {
	req := rs.req
	rs.t0 = time.Now()
	rng := rand.New(rand.NewPCG(req.Seed, 7))
	var wg sync.WaitGroup
	seq := 0

	fire := func(step int, sched time.Time) {
		cat := s.pickCategory(req.Mix, rng)
		p := s.buildPayment(rs, seq, cat, rng)
		rec := &record{recData: recData{cat: uint8(cat), step: int16(step), sched: int64(sched.Sub(rs.t0)), send: -1, ack: -1, recv: -1}}
		rs.stepsMu.Lock()
		rs.recs = append(rs.recs, rec)
		rs.stepsMu.Unlock()
		if lag := int64(time.Since(sched)); lag > rs.genLagNs.Load() {
			rs.genLagNs.Store(lag)
		}
		body, err := s.Mapper.BuildInbound(p)
		if err != nil {
			rec.ackCode = -2
			return
		}
		seq++
		dup := s.Sim.Pick("send")
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.deliver(ctx, rs, rec, body, true)
			if dup != nil && dup.Mode == "dup_seq" {
				s.deliver(ctx, rs, rec, body, false)
			}
		}()
		if dup != nil && dup.Mode == "dup_conc" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(rng2(3)) * time.Millisecond)
				s.deliver(ctx, rs, rec, body, false)
			}()
		}
	}

	segment := func(step int, rate float64, dur time.Duration) {
		start := time.Now()
		interval := time.Duration(float64(time.Second) / rate)
		for j := 0; ; j++ {
			sched := start.Add(time.Duration(j) * interval)
			if sched.Sub(start) >= dur || rs.stop.Load() || ctx.Err() != nil {
				return
			}
			if d := time.Until(sched); d > 0 {
				time.Sleep(d)
			}
			fire(step, sched)
		}
	}

	if su := req.StepUp; su != nil {
		if su.StepDurS <= 0 {
			su.StepDurS = 60
		}
		for k := 0; !rs.stop.Load(); k++ {
			rate := su.StartRate + float64(k)*su.Step
			if su.MaxRate > 0 && rate > su.MaxRate {
				rs.stepsMu.Lock()
				rs.capped = true
				rs.ceiling = su.StartRate + float64(k-1)*su.Step
				rs.stepsMu.Unlock()
				break
			}
			segment(k, rate, time.Duration(su.StepDurS)*time.Second)
			go s.evalStep(ctx, rs, k, rate)
		}
		// let the last evaluations run
		time.Sleep(2*time.Duration(req.SLAms)*time.Millisecond + 500*time.Millisecond)
	} else {
		segment(0, req.Rate, time.Duration(req.WarmupS+req.DurationS)*time.Second)
	}

	rs.state.Store("draining")
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(time.Duration(req.DrainS) * time.Second):
	}
	deadline := time.Now().Add(time.Duration(req.DrainS) * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil && !s.allAnswered(rs) {
		time.Sleep(100 * time.Millisecond)
	}
	rs.state.Store("done")
}

func rng2(n int) int { return rand.IntN(n) }

func (s *Server) allAnswered(rs *runState) bool {
	rs.stepsMu.Lock()
	defer rs.stepsMu.Unlock()
	for _, r := range rs.recs {
		r.mu.Lock()
		pending := r.sent && r.ackCode == 202 && r.recv < 0
		r.mu.Unlock()
		if pending {
			return false
		}
	}
	return true
}

// deliver posts the ibps.101. The first delivery stamps send/ack; duplicates only count their outcome.
func (s *Server) deliver(ctx context.Context, rs *runState, rec *record, body []byte, first bool) {
	url := rs.req.IngressURL + "/ingress/ibps101"
	if rs.req.Mode == "sync" {
		url += "?mode=sync"
	}
	t := time.Now()
	if first {
		rec.mu.Lock()
		rec.send, rec.sent = int64(t.Sub(rs.t0)), true
		rec.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	code := int32(-1)
	if err == nil {
		req.Header.Set("Content-Type", "application/xml")
		if resp, err := s.HTTP.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			code = int32(resp.StatusCode)
		}
	}
	ackAt := int64(time.Since(rs.t0))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if first {
		rec.ack, rec.ackCode = ackAt, code
		return
	}
	rec.dupSent++
	if code != 202 && code != 200 {
		rec.dupBad++
	}
}

// ---- step-up evaluation ------------------------------------------------------------------------------

type StepStat struct {
	Step       int     `json:"step"`
	Rate       float64 `json:"rate"`
	Sent       int     `json:"sent"`
	Unanswered int     `json:"unanswered"`
	IngressErr int     `json:"ingressErrors"`
	P99Ms      float64 `json:"p99Ms"`
	ErrorRate  float64 `json:"errorRate"`
	Breach     bool    `json:"breach"`
	Reason     string  `json:"reason,omitempty"`
}

// evalStep judges a step once every payment of it has had 2xSLA to be answered. It runs pipelined with
// the next step; a breach stops the generator and fixes the ceiling at the previous step's rate.
func (s *Server) evalStep(ctx context.Context, rs *runState, k int, rate float64) {
	sla := time.Duration(rs.req.SLAms) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * sla):
	}
	st := StepStat{Step: k, Rate: rate}
	var lat []float64
	rs.stepsMu.Lock()
	recs := append([]*record(nil), rs.recs...)
	rs.stepsMu.Unlock()
	for _, r := range recs {
		r.mu.Lock()
		if int(r.step) == k {
			st.Sent++
			switch {
			case r.ackCode != 202 && r.ackCode != 200:
				st.IngressErr++
				lat = append(lat, float64(2*sla)/1e6)
			case r.recv < 0:
				st.Unanswered++
				lat = append(lat, float64(2*sla)/1e6)
			default:
				lat = append(lat, float64(r.recv-r.sched)/1e6)
			}
		}
		r.mu.Unlock()
	}
	if st.Sent > 0 {
		st.P99Ms = percentile(lat, 99)
		st.ErrorRate = float64(st.IngressErr+st.Unanswered) / float64(st.Sent)
	}
	switch {
	case st.P99Ms > float64(sla)/1e6:
		st.Breach, st.Reason = true, "p99 above SLA"
	case st.ErrorRate > 0.001:
		st.Breach, st.Reason = true, "error rate above 0.1%"
	}
	rs.stepsMu.Lock()
	rs.steps = append(rs.steps, st)
	if st.Breach && !rs.breached {
		rs.breached = true
		if su := rs.req.StepUp; su != nil && k > 0 {
			rs.ceiling = su.StartRate + float64(k-1)*su.Step
		} else {
			rs.ceiling = 0
		}
		rs.stop.Store(true)
	}
	rs.stepsMu.Unlock()
}

// ---- results -------------------------------------------------------------------------------------------

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	rank := int(float64(len(s))*p/100 + 0.999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

type LatencyStats struct {
	N     int     `json:"n"`
	P50   float64 `json:"p50Ms"`
	P95   float64 `json:"p95Ms"`
	P99   float64 `json:"p99Ms"`
	P999  float64 `json:"p999Ms"`
	Max   float64 `json:"maxMs"`
	MeanM float64 `json:"meanMs"`
}

func stats(v []float64) LatencyStats {
	if len(v) == 0 {
		return LatencyStats{}
	}
	var sum, max float64
	for _, x := range v {
		sum += x
		if x > max {
			max = x
		}
	}
	return LatencyStats{N: len(v), P50: percentile(v, 50), P95: percentile(v, 95), P99: percentile(v, 99),
		P999: percentile(v, 99.9), Max: max, MeanM: sum / float64(len(v))}
}

type CatStats struct {
	Sent     int            `json:"sent"`
	Received int            `json:"received"`
	AsExpect int            `json:"asExpected"`
	Mismatch int            `json:"mismatch"`
	Outcomes map[string]int `json:"outcomes"`
	Latency  LatencyStats   `json:"latency"`
}

type Second struct {
	T    int     `json:"t"`
	Sent int     `json:"sent"`
	Recv int     `json:"recv"`
	P50  float64 `json:"p50Ms"`
	P99  float64 `json:"p99Ms"`
}

type Summary struct {
	RunID          string               `json:"runId"`
	State          string               `json:"state"`
	Mode           string               `json:"mode"`
	Rate           float64              `json:"rate"`
	Sent           int                  `json:"sent"`
	Measured       int                  `json:"measured"`
	IngressOK      int                  `json:"ingressOk"`
	IngressErr     int                  `json:"ingressErr"`
	Received       int                  `json:"received"`
	Unanswered     int                  `json:"unanswered"`
	Latency        LatencyStats         `json:"latency"`
	AckLatency     LatencyStats         `json:"ackLatency"`
	SLABreach      int                  `json:"slaBreach"`
	SLABreachRate  float64              `json:"slaBreachRate"`
	AchievedTPS    float64              `json:"achievedTps"`
	Outcomes       map[string]int       `json:"outcomes"`
	ByCategory     map[string]*CatStats `json:"byCategory"`
	MultiReceipts  int                  `json:"multiReceipts"`
	DoubleAnswered int                  `json:"doubleAnswered"`
	DupDeliveries  int                  `json:"dupDeliveries"`
	DupBadAcks     int                  `json:"dupBadAcks"`
	StallCount     int                  `json:"stallCount"`
	StallFirstMs   float64              `json:"stallFirstSchedMs"`
	StallLastMs    float64              `json:"stallLastRecvMs"`
	StallSpanMs    float64              `json:"stallSpanMs"`
	GenLagMaxMs    float64              `json:"genLagMaxMs"`
	Steps          []StepStat           `json:"steps,omitempty"`
	CeilingRate    float64              `json:"ceilingRate,omitempty"`
	CeilingCapped  bool                 `json:"ceilingCapped,omitempty"`
	Timeline       []Second             `json:"timeline"`
}

// Summarize computes the run summary. Latency is measured from the scheduled send time (open model), over
// payments scheduled after the warm-up.
func (rs *runState) Summarize() Summary {
	req := rs.req
	st, _ := rs.state.Load().(string)
	sum := Summary{RunID: req.RunID, State: st, Mode: req.Mode, Rate: req.Rate,
		Outcomes: map[string]int{}, ByCategory: map[string]*CatStats{}, GenLagMaxMs: float64(rs.genLagNs.Load()) / 1e6}
	warm := int64(req.WarmupS) * int64(time.Second)
	sla := float64(req.SLAms)
	stall := float64(req.StallMs)
	var lat, ack []float64
	perCat := map[string][]float64{}
	perSec := map[int][]float64{}
	sentSec := map[int]int{}
	var maxSec int
	rs.stepsMu.Lock()
	recs := append([]*record(nil), rs.recs...)
	steps := append([]StepStat(nil), rs.steps...)
	sum.CeilingRate, sum.CeilingCapped = rs.ceiling, rs.capped
	rs.stepsMu.Unlock()
	sort.Slice(steps, func(i, j int) bool { return steps[i].Step < steps[j].Step })
	sum.Steps = steps
	firstStall, lastStall := -1.0, -1.0
	for _, r := range recs {
		r.mu.Lock()
		rr := r.recData
		r.mu.Unlock()
		sum.Sent++
		sum.DupDeliveries += int(rr.dupSent)
		sum.DupBadAcks += int(rr.dupBad)
		if rr.nReceipts > 1 {
			sum.MultiReceipts++
		}
		if rr.distinct > 0 {
			sum.DoubleAnswered++
		}
		if rr.ackCode == 202 || rr.ackCode == 200 {
			sum.IngressOK++
		} else {
			sum.IngressErr++
		}
		if rr.sched < warm {
			continue
		}
		sum.Measured++
		cat := Categories[rr.cat]
		cs := sum.ByCategory[cat.Name]
		if cs == nil {
			cs = &CatStats{Outcomes: map[string]int{}}
			sum.ByCategory[cat.Name] = cs
		}
		cs.Sent++
		sec := int((rr.sched - warm) / int64(time.Second))
		sentSec[sec]++
		if sec > maxSec {
			maxSec = sec
		}
		if rr.ack >= 0 && rr.send >= 0 {
			ack = append(ack, float64(rr.ack-rr.send)/1e6)
		}
		if rr.recv < 0 {
			if rr.ackCode == 202 || rr.ackCode == 200 {
				sum.Unanswered++
			}
			sum.SLABreach++
			continue
		}
		l := float64(rr.recv-rr.sched) / 1e6
		lat = append(lat, l)
		perCat[cat.Name] = append(perCat[cat.Name], l)
		rsec := int((rr.recv - warm) / int64(time.Second))
		if rsec >= 0 {
			perSec[rsec] = append(perSec[rsec], l)
			if rsec > maxSec {
				maxSec = rsec
			}
		}
		if l > sla {
			sum.SLABreach++
		}
		if l > stall {
			sum.StallCount++
			if s := float64(rr.sched-warm) / 1e6; firstStall < 0 || s < firstStall {
				firstStall = s
			}
			if e := float64(rr.recv-warm) / 1e6; e > lastStall {
				lastStall = e
			}
		}
		sum.Received++
		outcome := "ACCP"
		if !rr.accepted {
			outcome = rr.iso
		}
		sum.Outcomes[outcome]++
		cs.Received++
		cs.Outcomes[outcome]++
		if outcome == cat.Expect {
			cs.AsExpect++
		} else {
			cs.Mismatch++
		}
	}
	sum.Latency, sum.AckLatency = stats(lat), stats(ack)
	for name, v := range perCat {
		sum.ByCategory[name].Latency = stats(v)
	}
	if sum.Measured > 0 {
		sum.SLABreachRate = float64(sum.SLABreach) / float64(sum.Measured)
	}
	if req.DurationS > 0 {
		sum.AchievedTPS = float64(len(lat)) / float64(req.DurationS)
	}
	if firstStall >= 0 {
		sum.StallFirstMs, sum.StallLastMs, sum.StallSpanMs = firstStall, lastStall, lastStall-firstStall
	}
	for t := 0; t <= maxSec && t < 7200; t++ {
		sum.Timeline = append(sum.Timeline, Second{T: t, Sent: sentSec[t], Recv: len(perSec[t]),
			P50: percentile(perSec[t], 50), P99: percentile(perSec[t], 99)})
	}
	return sum
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	rs := s.current()
	if rs == nil {
		simx.JSON(w, http.StatusOK, map[string]any{"state": "idle"})
		return
	}
	rs.stepsMu.Lock()
	n := len(rs.recs)
	rs.stepsMu.Unlock()
	st, _ := rs.state.Load().(string)
	simx.JSON(w, http.StatusOK, map[string]any{"state": st, "runId": rs.req.RunID, "scheduled": n,
		"elapsedS": time.Since(rs.t0).Seconds(), "callbacks": s.callbk.Load(), "steps": len(rs.steps)})
}

func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	rs := s.current()
	if rs == nil {
		http.Error(w, "no run", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("detail") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		rs.stepsMu.Lock()
		recs := append([]*record(nil), rs.recs...)
		rs.stepsMu.Unlock()
		for i, rec := range recs {
			rec.mu.Lock()
			_ = enc.Encode(map[string]any{"seq": i, "cat": Categories[rec.cat].Name, "step": rec.step, "schedNs": rec.sched,
				"sendNs": rec.send, "ackNs": rec.ack, "recvNs": rec.recv, "ackCode": rec.ackCode, "accepted": rec.accepted,
				"iso": rec.iso, "receipts": rec.nReceipts, "distinct": rec.distinct})
			rec.mu.Unlock()
		}
		return
	}
	simx.JSON(w, http.StatusOK, rs.Summarize())
}
