// Package ingress is the engine-neutral HTTP front door: parse, dedup, record RECEIVED, start the durable
// workflow (idempotent by workflow ID), acknowledge. Only the Starter differs between engines.
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/steps"
)

// Starter is the only engine-specific part of ingress.
type Starter interface {
	// Start durably starts the payment workflow. It must be idempotent on in.Payment.WorkflowID():
	// a second call for the same payment must attach to the existing execution, never start another.
	Start(ctx context.Context, in model.PaymentInput) error
	// Await blocks until the workflow's outcome is known (sync mode only).
	Await(ctx context.Context, in model.PaymentInput) (model.Outcome, error)
}

type Handler struct {
	Deps    *steps.Deps
	Starter Starter
}

type ack struct {
	ReceiptID string             `json:"receiptId"`
	Duplicate bool               `json:"duplicate,omitempty"`
	State     model.PaymentState `json:"state,omitempty"`
	Reason    model.RejectReason `json:"reason,omitempty"`
	Accepted  *bool              `json:"accepted,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Routes registers /ingress/ibps101 and /healthz.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /ingress/ibps101", h.ibps101)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func (h *Handler) ibps101(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": string(model.ReasonFormatInvalid)})
		return
	}
	p, err := h.Deps.Mapper.InboundToPayment(raw)
	if err != nil {
		var fe *mapping.FormatError
		if errors.As(err, &fe) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"reason": string(model.ReasonFormatInvalid), "isoCode": model.ReasonFormatInvalid.ISOCode(), "detail": fe.Msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now()
	in := model.PaymentInput{Payment: p, ReceivedAt: now, Deadline: now.Add(h.Deps.Cfg.SLA.Budget)}

	first, err := h.Deps.Store.Register(r.Context(), p, now)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "register: " + err.Error()})
		return
	}
	a := ack{ReceiptID: p.WorkflowID(), Duplicate: !first}
	if !first {
		// A duplicate delivery gets the stored outcome. If the first delivery died before its workflow
		// was started, the payment is still RECEIVED, so start is retried (idempotent).
		snap, ok, err := h.Deps.Store.Get(r.Context(), p)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lookup: " + err.Error()})
			return
		}
		if ok {
			a.State, a.Reason = snap.State, snap.Reason
		}
		if ok && snap.State != model.StateReceived {
			writeJSON(w, http.StatusAccepted, a)
			return
		}
	}
	if err := h.Starter.Start(r.Context(), in); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "start: " + err.Error()})
		return
	}
	if r.URL.Query().Get("mode") == "sync" {
		out, err := h.Starter.Await(r.Context(), in)
		if err != nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "await: " + err.Error()})
			return
		}
		a.Accepted, a.Reason = &out.Accepted, out.Reason
		writeJSON(w, http.StatusOK, a)
		return
	}
	writeJSON(w, http.StatusAccepted, a)
}
