package aml

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bill/ibps-bench/sims/simx"
)

func newServer() (*Server, *http.ServeMux) {
	s := New(simx.New(0))
	s.FastReview, s.SlowReview = 60*time.Millisecond, 10*time.Second
	mux := http.NewServeMux()
	s.Routes(mux)
	return s, mux
}

func call(mux http.Handler, method, url, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
	return rec
}

func TestScreenResults(t *testing.T) {
	_, mux := newServer()
	cases := map[string]string{
		`{"debtorName":"PAYER 1","creditorName":"CUSTOMER 1"}`:   `"result":"CLEAR"`,
		`{"debtorName":"BLOCKED PARTY 01","creditorName":"C"}`:   `"listId":"WL-01"`,
		`{"debtorName":"P","creditorName":"BLOCKED PARTY 20"}`:   `"listId":"WL-20"`,
		`{"debtorName":"BLOCKED PARTY 21","creditorName":"C"}`:   `"result":"CLEAR"`,
		`{"debtorName":"BLOCKED PARTY XX","creditorName":"C"}`:   `"result":"CLEAR"`,
		`{"debtorName":"REVIEW-FAST 000123","creditorName":"C"}`: `"result":"REVIEW"`,
		`{"debtorName":"REVIEW-SLOW 000123","creditorName":"C"}`: `"caseId":"CASE-`,
	}
	for body, want := range cases {
		rec := call(mux, "POST", "/screen", body)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s -> %d %s, want %s", body, rec.Code, rec.Body, want)
		}
	}
	if rec := call(mux, "POST", "/screen", "nope"); rec.Code != 400 {
		t.Errorf("bad body: %d", rec.Code)
	}
}

func TestCaseLifecycle(t *testing.T) {
	_, mux := newServer()
	rec := call(mux, "POST", "/screen", `{"debtorName":"REVIEW-FAST 1","creditorName":"C"}`)
	id := rec.Body.String()[strings.Index(rec.Body.String(), "CASE-"):]
	id = id[:strings.Index(id, `"`)]
	if r := call(mux, "GET", "/cases/"+id, ""); !strings.Contains(r.Body.String(), `"OPEN"`) {
		t.Errorf("must start OPEN: %s", r.Body)
	}
	time.Sleep(80 * time.Millisecond)
	if r := call(mux, "GET", "/cases/"+id, ""); !strings.Contains(r.Body.String(), `"CLEARED"`) {
		t.Errorf("must auto-clear: %s", r.Body)
	}
	for _, bad := range []string{"nonsense", "CASE-x-1-2", "CASE-1-y-2"} {
		if r := call(mux, "GET", "/cases/"+bad, ""); r.Code != 404 {
			t.Errorf("%s: %d", bad, r.Code)
		}
	}
	if _, ok := CaseState("CASE-1-2", time.Now()); ok {
		t.Error("malformed case id must be rejected")
	}
}

func TestFaults(t *testing.T) {
	s, mux := newServer()
	call(mux, "POST", "/faults", `{"target":"screen","mode":"http503","durationMs":150}`)
	if r := call(mux, "POST", "/screen", `{}`); r.Code != 503 {
		t.Errorf("fault not applied: %d", r.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if r := call(mux, "POST", "/screen", `{}`); r.Code != 200 {
		t.Errorf("fault must expire: %d", r.Code)
	}
	call(mux, "POST", "/faults", `{"target":"cases","mode":"http503"}`)
	if r := call(mux, "GET", "/cases/CASE-1-1-1", ""); r.Code != 503 {
		t.Errorf("cases fault: %d", r.Code)
	}
	call(mux, "POST", "/faults", `{"target":"screen","mode":"latency","latencyMs":40}`)
	start := time.Now()
	call(mux, "POST", "/screen", `{}`)
	if time.Since(start) < 40*time.Millisecond {
		t.Error("latency fault not applied")
	}
	if r := call(mux, "DELETE", "/faults", ""); r.Code != 200 || s.Sim.Pick("screen") != nil {
		t.Error("DELETE /faults must clear")
	}
	if r := call(mux, "POST", "/faults", `{"mode":"x"}`); r.Code != 400 {
		t.Errorf("incomplete fault: %d", r.Code)
	}
	if r := call(mux, "GET", "/faults", ""); r.Code != 200 {
		t.Error("GET /faults")
	}
	if r := call(mux, "POST", "/config/latency", `{"ms":-1}`); r.Code != 400 {
		t.Error("negative latency must be rejected")
	}
	if r := call(mux, "POST", "/config/latency", `{"ms":0}`); r.Code != 200 {
		t.Error("latency config")
	}
	if r := call(mux, "GET", "/healthz", ""); r.Code != 200 {
		t.Error("healthz")
	}
}
