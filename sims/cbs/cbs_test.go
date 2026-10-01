package cbs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bill/ibps-bench/sims/simx"
)

// Contract tests need cbs-db (make up-v1/up-v2). They skip when it is not reachable.
func newServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := Open(ctx, "postgres://bench:bench@localhost:5434/cbs?sslmode=disable", simx.New(0))
	if err != nil {
		t.Skipf("cbs-db not reachable: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	if err := s.Seed(ctx, 20000); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	return s, mux
}

func call(h http.Handler, method, url, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
	return rec
}

func TestSuffixConvention(t *testing.T) {
	for suffix, want := range map[int]string{0: "ACTIVE", 8999: "ACTIVE", 9000: "", 9099: "", 9100: "CLOSED", 9199: "CLOSED",
		9200: "FROZEN", 9299: "FROZEN", 9300: "DORMANT", 9399: "DORMANT", 9400: "ACTIVE", 9999: "ACTIVE"} {
		if got := StatusForSuffix(suffix); got != want {
			t.Errorf("suffix %d: %q want %q", suffix, got, want)
		}
	}
	if SplitSuffix(AccountID(9123)) != 9123 || SplitSuffix("12") != -1 || SplitSuffix("abcd") != -1 {
		t.Error("SplitSuffix")
	}
}

func TestValidateContract(t *testing.T) {
	_, h := newServer(t)
	get := func(i int, name string) *httptest.ResponseRecorder {
		return call(h, "GET", "/accounts/"+AccountID(i)+"/validate?ccy=CNY&name="+strings.ReplaceAll(name, " ", "%20"), "")
	}
	if r := get(42, AccountName(42)); r.Code != 200 || !strings.Contains(r.Body.String(), "VALID") {
		t.Errorf("valid: %d %s", r.Code, r.Body)
	}
	for i, want := range map[int]string{9050: "ACCOUNT_NOT_FOUND", 9150: "CLOSED", 9250: "FROZEN", 9350: "DORMANT"} {
		if r := get(i, AccountName(i)); r.Code != 422 || !strings.Contains(r.Body.String(), want) {
			t.Errorf("account %d: %d %s want %s", i, r.Code, r.Body, want)
		}
	}
	if r := get(42, AccountName(42)+" X"); r.Code != 422 || !strings.Contains(r.Body.String(), "NAME_MISMATCH") {
		t.Errorf("name mismatch: %d %s", r.Code, r.Body)
	}
	// Settlement accounts are not customer accounts.
	if r := call(h, "GET", "/accounts/SETTLE-IBPS-0/validate?name=x", ""); r.Code != 422 {
		t.Errorf("settlement account must not validate as a customer: %d", r.Code)
	}
}

func postBody(ref string, amount int64, debit, credit string) string {
	b, _ := json.Marshal(postReq{PostingRef: ref, Legs: []leg{{debit, "D", amount}, {credit, "C", amount}}})
	return string(b)
}

func TestPostingContract(t *testing.T) {
	_, h := newServer(t)
	body := postBody("REF-1", 12345, "SETTLE-IBPS-3", AccountID(7))
	if r := call(h, "POST", "/postings", body); r.Code != 201 || !strings.Contains(r.Body.String(), "POSTED") {
		t.Fatalf("first post: %d %s", r.Code, r.Body)
	}
	if r := call(h, "POST", "/postings", body); r.Code != 200 || !strings.Contains(r.Body.String(), "ALREADY_POSTED") {
		t.Fatalf("second post must be ALREADY_POSTED: %d %s", r.Code, r.Body)
	}
	if r := call(h, "GET", "/postings/REF-1", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "POSTED") {
		t.Errorf("status query: %d %s", r.Code, r.Body)
	}
	if r := call(h, "GET", "/postings/NOPE", ""); r.Code != 404 || !strings.Contains(r.Body.String(), "NOT_FOUND") {
		t.Errorf("unknown ref: %d %s", r.Code, r.Body)
	}
	for name, b := range map[string]string{
		"amounts differ": `{"postingRef":"R2","legs":[{"account":"a","side":"D","amountFen":5},{"account":"b","side":"C","amountFen":6}]}`,
		"one leg":        `{"postingRef":"R3","legs":[{"account":"a","side":"D","amountFen":5}]}`,
		"two debits":     `{"postingRef":"R4","legs":[{"account":"a","side":"D","amountFen":5},{"account":"b","side":"D","amountFen":5}]}`,
		"zero":           `{"postingRef":"R5","legs":[{"account":"a","side":"D","amountFen":0},{"account":"b","side":"C","amountFen":0}]}`,
		"same account":   `{"postingRef":"R6","legs":[{"account":"a","side":"D","amountFen":5},{"account":"a","side":"C","amountFen":5}]}`,
		"no ref":         `{"legs":[{"account":"a","side":"D","amountFen":5},{"account":"b","side":"C","amountFen":5}]}`,
	} {
		if r := call(h, "POST", "/postings", b); r.Code != 409 || !strings.Contains(r.Body.String(), "UNBALANCED") {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if r := call(h, "POST", "/postings", "not json"); r.Code != 400 {
		t.Errorf("bad body: %d", r.Code)
	}
	// Credit in either leg order is accepted.
	rev, _ := json.Marshal(postReq{PostingRef: "REF-REV", Legs: []leg{{AccountID(8), "C", 100}, {"SETTLE-IBPS-1", "D", 100}}})
	if r := call(h, "POST", "/postings", string(rev)); r.Code != 201 {
		t.Errorf("reversed leg order: %d %s", r.Code, r.Body)
	}
}

func TestInvariantsAndReset(t *testing.T) {
	s, h := newServer(t)
	for i := 0; i < 20; i++ {
		call(h, "POST", "/postings", postBody("INV-"+string(rune('a'+i)), int64(100+i), "SETTLE-IBPS-"+string(rune('0'+i%4)), AccountID(i)))
	}
	var inv Invariants
	r := call(h, "GET", "/ledger/invariants", "")
	json.Unmarshal(r.Body.Bytes(), &inv)
	if !inv.Ok || inv.Postings != 20 || inv.Legs != 40 || inv.LegSum != 0 || inv.DuplicateRefs != 0 || !inv.BalancesReconcile {
		t.Errorf("invariants after 20 postings: %+v", inv)
	}
	// Corrupt the ledger: the check must notice.
	s.Pool.Exec(context.Background(), `INSERT INTO legs (posting_ref, account, signed_fen) VALUES ('INV-a','x',7)`)
	json.Unmarshal(call(h, "GET", "/ledger/invariants", "").Body.Bytes(), &inv)
	if inv.Ok || inv.LegSum != 7 || inv.PostingsNotTwoLegs != 1 {
		t.Errorf("corruption not detected: %+v", inv)
	}
	if r := call(h, "POST", "/admin/reset", ""); r.Code != 200 {
		t.Fatal("reset")
	}
	json.Unmarshal(call(h, "GET", "/ledger/invariants", "").Body.Bytes(), &inv)
	if !inv.Ok || inv.Postings != 0 {
		t.Errorf("after reset: %+v", inv)
	}
	if r := call(h, "POST", "/admin/seed?accounts=1000", ""); r.Code != 200 {
		t.Error("seed endpoint")
	}
}

func TestConcurrentSameRefPostsOnce(t *testing.T) {
	_, h := newServer(t)
	body := postBody("CONC-1", 500, "SETTLE-IBPS-2", AccountID(9))
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		go func() { codes <- call(h, "POST", "/postings", body).Code }()
	}
	created := 0
	for i := 0; i < 16; i++ {
		switch <-codes {
		case 201:
			created++
		case 200:
		default:
			t.Error("unexpected status")
		}
	}
	if created != 1 {
		t.Errorf("exactly one request may create the posting, got %d", created)
	}
	var inv Invariants
	json.Unmarshal(call(h, "GET", "/ledger/invariants", "").Body.Bytes(), &inv)
	if inv.Postings != 1 || inv.Legs != 2 || !inv.Ok {
		t.Errorf("ledger after 16 concurrent posts: %+v", inv)
	}
}

func TestFaultsOnCBS(t *testing.T) {
	s, h := newServer(t)
	call(h, "POST", "/faults", `{"target":"validate","mode":"http503"}`)
	if r := call(h, "GET", "/accounts/"+AccountID(1)+"/validate?name=x", ""); r.Code != 503 {
		t.Errorf("validate 503: %d", r.Code)
	}
	call(h, "DELETE", "/faults", "")
	call(h, "POST", "/faults", `{"target":"validate","mode":"latency","latencyMs":40}`)
	start := time.Now()
	call(h, "GET", "/accounts/"+AccountID(1)+"/validate?name=x", "")
	if time.Since(start) < 40*time.Millisecond {
		t.Error("validate latency fault")
	}
	call(h, "DELETE", "/faults", "")

	call(h, "POST", "/faults", `{"target":"post","mode":"http503"}`)
	if r := call(h, "POST", "/postings", postBody("F-1", 5, "SETTLE-IBPS-0", AccountID(2))); r.Code != 503 {
		t.Errorf("post 503: %d", r.Code)
	}
	call(h, "DELETE", "/faults", "")

	// I5: hangs, then abandons without committing.
	call(h, "POST", "/faults", `{"target":"post","mode":"timeout_before_commit","latencyMs":60}`)
	r := call(h, "POST", "/postings", postBody("F-2", 5, "SETTLE-IBPS-0", AccountID(2)))
	if r.Code != 503 {
		t.Errorf("timeout_before_commit: %d", r.Code)
	}
	if g := call(h, "GET", "/postings/F-2", ""); g.Code != 404 {
		t.Errorf("I5 must not commit: %d", g.Code)
	}
	call(h, "DELETE", "/faults", "")

	// I4: commits, then the connection is dropped without a response. Use a real server to see the drop.
	call(h, "POST", "/faults", `{"target":"post","mode":"commit_then_drop"}`)
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/postings", "application/json", strings.NewReader(postBody("F-3", 5, "SETTLE-IBPS-0", AccountID(2))))
	if err == nil {
		resp.Body.Close()
		t.Errorf("I4: client must see a dropped connection, got %d", resp.StatusCode)
	}
	if g := call(h, "GET", "/postings/F-3", ""); g.Code != 200 {
		t.Errorf("I4 must have committed: %d", g.Code)
	}
	_ = s
}

func TestStatsCountCalls(t *testing.T) {
	_, h := newServer(t)
	call(h, "GET", "/accounts/"+AccountID(1)+"/validate?name=x", "")
	call(h, "POST", "/postings", postBody("ST-1", 5, "SETTLE-IBPS-0", AccountID(2)))
	call(h, "GET", "/postings/ST-1", "")
	call(h, "GET", "/postings/ST-1", "")
	var st map[string]int64
	json.Unmarshal(call(h, "GET", "/stats", "").Body.Bytes(), &st)
	if st["validates"] != 1 || st["posts"] != 1 || st["statusQueries"] != 2 {
		t.Errorf("stats %v", st)
	}
	call(h, "POST", "/admin/reset", "")
	json.Unmarshal(call(h, "GET", "/stats", "").Body.Bytes(), &st)
	if st["validates"] != 0 || st["posts"] != 0 {
		t.Errorf("reset must zero the counters: %v", st)
	}
}
