package model

import "testing"

func TestFormatFen(t *testing.T) {
	for in, want := range map[int64]string{0: "0.00", 5: "0.05", 100: "1.00", 123456: "1234.56", -250: "-2.50"} {
		if got := FormatFen(in); got != want {
			t.Errorf("FormatFen(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestReasonISOCodes(t *testing.T) {
	want := map[RejectReason]string{
		ReasonAccountNotFound: "AC01", ReasonAccountClosed: "AC04", ReasonAccountFrozen: "AC06",
		ReasonAccountDormant: "AC06", ReasonNameMismatch: "BE01", ReasonCurrencyNotAllow: "AM03",
		ReasonAmountOverLimit: "AM02", ReasonAMLHit: "RR04", ReasonAMLReviewTimeout: "RR04",
		ReasonSLATimeout: "AB05", ReasonWrongReceiver: "AGNT", ReasonFormatInvalid: "FF01",
	}
	for r, code := range want {
		if r.ISOCode() != code {
			t.Errorf("%s -> %q, want %q", r, r.ISOCode(), code)
		}
	}
	if RejectReason("nope").ISOCode() != "" {
		t.Error("unknown reason must map to empty code")
	}
}

func TestStateRankAndTerminal(t *testing.T) {
	order := []PaymentState{StateReceived, StateValidated, StateScreened, StateSettled, StateAcceptedSent}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Errorf("%s must rank above %s", order[i], order[i-1])
		}
	}
	if !StateAcceptedSent.Terminal() || !StateRejectedSent.Terminal() || StateRejected.Terminal() {
		t.Error("terminal states wrong")
	}
	if StateRejectedSent.Rank() <= StateRejected.Rank() {
		t.Error("REJECTED_SENT must outrank REJECTED")
	}
}

func TestKeysAreDeterministic(t *testing.T) {
	p := Payment{MsgID: "M1", EndToEndID: "E1", InstgAgent: "102100099996"}
	if p.PostingRef() != "IBPS-102100099996-M1-E1" || p.WorkflowID() != "pay-M1-E1" || p.Key() != "M1|E1" {
		t.Fatalf("unexpected keys: %s %s %s", p.PostingRef(), p.WorkflowID(), p.Key())
	}
}

func TestUnknownStateRank(t *testing.T) {
	if PaymentState("BOGUS").Rank() != 0 || PaymentState("BOGUS").Terminal() {
		t.Error("unknown state must rank 0 and not be terminal")
	}
}
