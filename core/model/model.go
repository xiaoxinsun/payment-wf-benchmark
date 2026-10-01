// Package model holds the canonical payment types shared by every component.
// It has no engine imports and no I/O.
package model

import (
	"fmt"
	"time"
)

// PaymentState is persisted in app-db in every variant (same enum).
type PaymentState string

const (
	StateReceived     PaymentState = "RECEIVED"
	StateValidated    PaymentState = "VALIDATED"
	StateScreened     PaymentState = "SCREENED"
	StateSettled      PaymentState = "SETTLED"
	StateAcceptedSent PaymentState = "ACCEPTED_SENT"
	StateRejected     PaymentState = "REJECTED"
	StateRejectedSent PaymentState = "REJECTED_SENT"
)

// Rank orders states so RecordState can refuse to move backwards.
func (s PaymentState) Rank() int {
	switch s {
	case StateReceived:
		return 1
	case StateValidated:
		return 2
	case StateScreened:
		return 3
	case StateSettled:
		return 4
	case StateRejected:
		return 4
	case StateAcceptedSent:
		return 5
	case StateRejectedSent:
		return 5
	}
	return 0
}

// Terminal reports whether no further transition is expected.
func (s PaymentState) Terminal() bool { return s == StateAcceptedSent || s == StateRejectedSent }

// RejectReason is the internal reason code (see the plan's reject table).
type RejectReason string

const (
	ReasonAccountNotFound  RejectReason = "ACCOUNT_NOT_FOUND"
	ReasonAccountClosed    RejectReason = "ACCOUNT_CLOSED"
	ReasonAccountFrozen    RejectReason = "ACCOUNT_FROZEN"
	ReasonAccountDormant   RejectReason = "ACCOUNT_DORMANT"
	ReasonNameMismatch     RejectReason = "NAME_MISMATCH"
	ReasonCurrencyNotAllow RejectReason = "CURRENCY_NOT_ALLOWED"
	ReasonAmountOverLimit  RejectReason = "AMOUNT_OVER_LIMIT"
	ReasonAMLHit           RejectReason = "AML_HIT"
	ReasonAMLReviewTimeout RejectReason = "AML_REVIEW_TIMEOUT"
	ReasonSLATimeout       RejectReason = "SLA_TIMEOUT"
	ReasonWrongReceiver    RejectReason = "WRONG_RECEIVER"
	ReasonFormatInvalid    RejectReason = "FORMAT_INVALID"
)

var isoCodes = map[RejectReason]string{
	ReasonAccountNotFound:  "AC01",
	ReasonAccountClosed:    "AC04",
	ReasonAccountFrozen:    "AC06",
	ReasonAccountDormant:   "AC06",
	ReasonNameMismatch:     "BE01",
	ReasonCurrencyNotAllow: "AM03",
	ReasonAmountOverLimit:  "AM02",
	ReasonAMLHit:           "RR04",
	ReasonAMLReviewTimeout: "RR04",
	ReasonSLATimeout:       "AB05",
	ReasonWrongReceiver:    "AGNT",
	ReasonFormatInvalid:    "FF01",
}

// ISOCode returns the ISO 20022 reason code for the reason, or "" if unknown.
func (r RejectReason) ISOCode() string { return isoCodes[r] }

// Payment is the canonical projection of a pacs.008 (one transaction). Amounts are integer fen.
type Payment struct {
	MsgID          string    `json:"msgId"`
	EndToEndID     string    `json:"e2eId"`
	TxID           string    `json:"txId"`
	CreDtTm        time.Time `json:"creDtTm"`
	InstgAgent     string    `json:"instgAgent"`
	InstdAgent     string    `json:"instdAgent"`
	AmountFen      int64     `json:"amountFen"`
	Currency       string    `json:"ccy"`
	SettlementDate string    `json:"sttlmDt"`
	BizTp          string    `json:"bizTp"`
	BizKind        string    `json:"bizKind"`
	DebtorName     string    `json:"dbtrNm"`
	DebtorAcct     string    `json:"dbtrAcct"`
	DebtorBank     string    `json:"dbtrBk"`
	CreditorName   string    `json:"cdtrNm"`
	CreditorAcct   string    `json:"cdtrAcct"`
	CreditorBank   string    `json:"cdtrBk"`
	Remark         string    `json:"remark"`
}

// Key is the dedup key: message ID plus end-to-end ID.
func (p Payment) Key() string { return p.MsgID + "|" + p.EndToEndID }

// WorkflowID is derived from the dedup key and is identical in both engines.
func (p Payment) WorkflowID() string { return "pay-" + p.MsgID + "-" + p.EndToEndID }

// PostingRef is the CBS idempotency key. It is never regenerated on retry.
func (p Payment) PostingRef() string {
	return fmt.Sprintf("IBPS-%s-%s-%s", p.InstgAgent, p.MsgID, p.EndToEndID)
}

// PaymentInput is the workflow input: identical payload in every engine.
type PaymentInput struct {
	Payment    Payment   `json:"payment"`
	ReceivedAt time.Time `json:"receivedAt"`
	Deadline   time.Time `json:"deadline"`
}

// Outcome is the final business decision for a payment.
type Outcome struct {
	Accepted  bool         `json:"accepted"`
	Reason    RejectReason `json:"reason,omitempty"`
	DecidedAt time.Time    `json:"decidedAt"`
}

// FormatFen renders integer fen as a 2 dp decimal string.
func FormatFen(fen int64) string {
	sign := ""
	if fen < 0 {
		sign, fen = "-", -fen
	}
	return fmt.Sprintf("%s%d.%02d", sign, fen/100, fen%100)
}
