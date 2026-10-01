package ingress

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
	"github.com/bill/ibps-bench/core/steps"
)

type fakeStarter struct {
	starts atomic.Int32
	err    error
	out    model.Outcome
}

func (f *fakeStarter) Start(context.Context, model.PaymentInput) error { f.starts.Add(1); return f.err }
func (f *fakeStarter) Await(context.Context, model.PaymentInput) (model.Outcome, error) {
	return f.out, f.err
}

func newHandler(t *testing.T) (*Handler, *fakeStarter, http.Handler) {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := steps.OpenStore(ctx, "postgres://bench:bench@localhost:5435/app?sslmode=disable")
	if err != nil {
		t.Skipf("app-db not reachable: %v", err)
	}
	t.Cleanup(st.Pool.Close)
	st.Reset(ctx)
	fs := &fakeStarter{}
	h := &Handler{Deps: &steps.Deps{Cfg: cfg, Mapper: mapping.SyntheticMapper{}, Store: st}, Starter: fs}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, fs, mux
}

func body(t *testing.T, id string) string {
	raw, err := mapping.SyntheticMapper{}.BuildInbound(model.Payment{
		MsgID: id, EndToEndID: "E-" + id, TxID: "T", CreDtTm: time.Now(), InstgAgent: "102100099996", InstdAgent: "313000000000",
		AmountFen: 100, Currency: "CNY", CreditorName: "C", CreditorAcct: "6222000000000042"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func post(mux http.Handler, url, b string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, strings.NewReader(b)))
	return rec
}

func TestAckAndDuplicate(t *testing.T) {
	h, fs, mux := newHandler(t)
	rec := post(mux, "/ingress/ibps101", body(t, "A1"))
	if rec.Code != 202 || fs.starts.Load() != 1 {
		t.Fatalf("first delivery: %d starts=%d %s", rec.Code, fs.starts.Load(), rec.Body)
	}
	var a ack
	json.Unmarshal(rec.Body.Bytes(), &a)
	if a.ReceiptID != "pay-A1-E-A1" || a.Duplicate {
		t.Errorf("ack %+v", a)
	}
	// Duplicate while still RECEIVED: start is retried (idempotent in the engine), flagged duplicate.
	rec = post(mux, "/ingress/ibps101", body(t, "A1"))
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 202 || !a.Duplicate || fs.starts.Load() != 2 {
		t.Errorf("dup while RECEIVED: %d %+v starts=%d", rec.Code, a, fs.starts.Load())
	}
	// Duplicate after the payment progressed: stored outcome returned, no new start.
	p, _ := mapping.SyntheticMapper{}.InboundToPayment([]byte(body(t, "A1")))
	h.Deps.Store.RecordState(context.Background(), p, model.StateRejectedSent, model.ReasonAMLHit, time.Now())
	rec = post(mux, "/ingress/ibps101", body(t, "A1"))
	a = ack{}
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 202 || !a.Duplicate || a.State != model.StateRejectedSent || a.Reason != model.ReasonAMLHit || fs.starts.Load() != 2 {
		t.Errorf("dup after terminal: %d %+v starts=%d", rec.Code, a, fs.starts.Load())
	}
}

func TestFormatInvalidAndHealth(t *testing.T) {
	_, fs, mux := newHandler(t)
	rec := post(mux, "/ingress/ibps101", "<garbage")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "FF01") || fs.starts.Load() != 0 {
		t.Errorf("bad format: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Error("healthz")
	}
}

func TestStartFailureAndSyncMode(t *testing.T) {
	_, fs, mux := newHandler(t)
	fs.err = errors.New("engine down")
	if rec := post(mux, "/ingress/ibps101", body(t, "B1")); rec.Code != 503 {
		t.Errorf("start failure must be 503, got %d", rec.Code)
	}
	fs.err = nil
	fs.out = model.Outcome{Accepted: true}
	rec := post(mux, "/ingress/ibps101?mode=sync", body(t, "B2"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Errorf("sync mode: %d %s", rec.Code, rec.Body)
	}
	fs.err = errors.New("timeout")
	if rec := post(mux, "/ingress/ibps101?mode=sync", body(t, "B3")); rec.Code != 503 {
		// Start fails first in this fake, so 503; the await path is covered below.
		t.Errorf("got %d", rec.Code)
	}
}

type awaitFails struct{ fakeStarter }

func (*awaitFails) Await(context.Context, model.PaymentInput) (model.Outcome, error) {
	return model.Outcome{}, errors.New("deadline")
}

func TestSyncAwaitFailure(t *testing.T) {
	h, _, _ := newHandler(t)
	h.Starter = &awaitFails{}
	mux := http.NewServeMux()
	h.Routes(mux)
	if rec := post(mux, "/ingress/ibps101?mode=sync", body(t, "C1")); rec.Code != 504 {
		t.Errorf("await failure must be 504, got %d", rec.Code)
	}
}

func TestClosedStoreIs503(t *testing.T) {
	h, _, mux := newHandler(t)
	h.Deps.Store.Pool.Close()
	if rec := post(mux, "/ingress/ibps101", body(t, "D1")); rec.Code != 503 {
		t.Errorf("db down must be 503, got %d", rec.Code)
	}
}
