package mapping

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bill/ibps-bench/core/model"
)

// CST is the +08:00 zone used on every timestamp (a fixed zone avoids depending on tzdata in containers).
var CST = time.FixedZone("CST", 8*3600)

const remarkMax = 140

// FormatError marks a message that must be rejected at ingress with FF01.
type FormatError struct{ Msg string }

func (e *FormatError) Error() string { return "format invalid: " + e.Msg }

// Receipt is what the NPC side learns from an ibps.102.
type Receipt struct {
	MsgID, OrigMsgID, OrigEndToEndID string
	Accepted                         bool
	IsoReason                        string
	PrcDtTm                          time.Time
}

// MessageMapper is the seam for the bank's real IBPS spec (both directions).
type MessageMapper interface {
	// InboundToPayment maps raw ibps.101 -> pacs.008 -> canonical payment.
	InboundToPayment(raw []byte) (model.Payment, error)
	// OutboundReceipt maps a decision -> pacs.002 -> raw ibps.102 with a deterministic receipt MsgId.
	OutboundReceipt(p model.Payment, o model.Outcome) (raw []byte, msgID string, err error)
	// BuildInbound renders a canonical payment as raw ibps.101 (simulator side).
	BuildInbound(p model.Payment) ([]byte, error)
	// ParseReceipt reads a raw ibps.102 (simulator side).
	ParseReceipt(raw []byte) (Receipt, error)
}

// SyntheticMapper implements MessageMapper for the synthetic schema.
type SyntheticMapper struct {
	// RejectCodes maps ISO reason code -> IBPS proprietary code. Missing entries pass the ISO code through.
	RejectCodes map[string]string
}

var _ MessageMapper = SyntheticMapper{}

// ---- amounts -------------------------------------------------------------------------------------

// ParseFen parses a decimal string with at most 2 decimals into integer fen.
func ParseFen(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, &FormatError{"amount: " + strconv.Quote(s)}
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 || (hasDot && frac == "") {
		return 0, &FormatError{"amount: " + strconv.Quote(s)}
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > (1<<62)/100 {
		return 0, &FormatError{"amount: " + strconv.Quote(s)}
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, &FormatError{"amount: " + strconv.Quote(s)}
	}
	return w*100 + f, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- ibps.101 <-> pacs.008 -----------------------------------------------------------------------

// IBPS101ToPacs008 maps the synthetic inbound message to pacs.008.001.08 following the plan's table.
func IBPS101ToPacs008(m IBPS101) Pacs008 {
	var d Pacs008
	d.GrpHdr.MsgID = m.MsgID
	d.GrpHdr.CreDtTm = m.CreDtTm
	d.GrpHdr.NbOfTxs = 1
	d.GrpHdr.SttlmInf.SttlmMtd = "CLRG"
	d.GrpHdr.SttlmInf.ClrSys = "IBPS"
	d.GrpHdr.InstgAgt.MmbID = m.InstgDrctPty
	d.GrpHdr.InstdAgt.MmbID = m.InstdDrctPty
	d.Tx.EndToEndID = m.EndToEndID
	d.Tx.TxID = m.TxID
	d.Tx.CtgyPurp = m.BizTp
	d.Tx.Amt = amount{Ccy: m.Ccy, Value: m.Amt}
	d.Tx.SttlmDt = m.SttlmDt
	d.Tx.ChrgBr = "SLEV"
	d.Tx.Dbtr.Nm = m.DbtrNm
	d.Tx.DbtrAcct.ID = m.DbtrAcct
	d.Tx.DbtrAgt.MmbID = m.DbtrBk
	d.Tx.CdtrAgt.MmbID = m.CdtrBk
	d.Tx.Cdtr.Nm = m.CdtrNm
	d.Tx.CdtrAcct.ID = m.CdtrAcct
	d.Tx.Purp = m.BizKind
	d.Tx.Ustrd = truncateRunes(m.Remark, remarkMax)
	return d
}

// Pacs008ToIBPS101 is the reverse mapping (used to generate test traffic from a canonical pacs.008).
func Pacs008ToIBPS101(d Pacs008) IBPS101 {
	return IBPS101{
		MsgID: d.GrpHdr.MsgID, CreDtTm: d.GrpHdr.CreDtTm,
		InstgDrctPty: d.GrpHdr.InstgAgt.MmbID, InstdDrctPty: d.GrpHdr.InstdAgt.MmbID,
		EndToEndID: d.Tx.EndToEndID, TxID: d.Tx.TxID, Amt: d.Tx.Amt.Value, Ccy: d.Tx.Amt.Ccy,
		SttlmDt: d.Tx.SttlmDt, BizTp: d.Tx.CtgyPurp, BizKind: d.Tx.Purp,
		DbtrNm: d.Tx.Dbtr.Nm, DbtrAcct: d.Tx.DbtrAcct.ID, DbtrBk: d.Tx.DbtrAgt.MmbID,
		CdtrNm: d.Tx.Cdtr.Nm, CdtrAcct: d.Tx.CdtrAcct.ID, CdtrBk: d.Tx.CdtrAgt.MmbID,
		Remark: d.Tx.Ustrd,
	}
}

// Pacs008ToPayment projects pacs.008 onto the canonical model, validating required fields.
func Pacs008ToPayment(d Pacs008) (model.Payment, error) {
	req := map[string]string{
		"MsgId": d.GrpHdr.MsgID, "EndToEndId": d.Tx.EndToEndID, "TxId": d.Tx.TxID,
		"InstgDrctPty": d.GrpHdr.InstgAgt.MmbID, "InstdDrctPty": d.GrpHdr.InstdAgt.MmbID,
		"CdtrAcct": d.Tx.CdtrAcct.ID, "CdtrNm": d.Tx.Cdtr.Nm, "Ccy": d.Tx.Amt.Ccy,
	}
	for k, v := range req {
		if strings.TrimSpace(v) == "" {
			return model.Payment{}, &FormatError{"missing " + k}
		}
	}
	if len(d.GrpHdr.MsgID) > 35 || len(d.Tx.EndToEndID) > 35 {
		return model.Payment{}, &FormatError{"identifier longer than 35 characters"}
	}
	at, err := time.Parse(time.RFC3339, d.GrpHdr.CreDtTm)
	if err != nil {
		return model.Payment{}, &FormatError{"CreDtTm: " + err.Error()}
	}
	fen, err := ParseFen(d.Tx.Amt.Value)
	if err != nil {
		return model.Payment{}, err
	}
	return model.Payment{
		MsgID: d.GrpHdr.MsgID, EndToEndID: d.Tx.EndToEndID, TxID: d.Tx.TxID, CreDtTm: at,
		InstgAgent: d.GrpHdr.InstgAgt.MmbID, InstdAgent: d.GrpHdr.InstdAgt.MmbID,
		AmountFen: fen, Currency: d.Tx.Amt.Ccy, SettlementDate: d.Tx.SttlmDt,
		BizTp: d.Tx.CtgyPurp, BizKind: d.Tx.Purp,
		DebtorName: d.Tx.Dbtr.Nm, DebtorAcct: d.Tx.DbtrAcct.ID, DebtorBank: d.Tx.DbtrAgt.MmbID,
		CreditorName: d.Tx.Cdtr.Nm, CreditorAcct: d.Tx.CdtrAcct.ID, CreditorBank: d.Tx.CdtrAgt.MmbID,
		Remark: d.Tx.Ustrd,
	}, nil
}

// PaymentToPacs008 renders a canonical payment as pacs.008.
func PaymentToPacs008(p model.Payment) Pacs008 {
	return IBPS101ToPacs008(paymentToIBPS101(p))
}

func paymentToIBPS101(p model.Payment) IBPS101 {
	return IBPS101{
		MsgID: p.MsgID, CreDtTm: p.CreDtTm.In(CST).Format(time.RFC3339),
		InstgDrctPty: p.InstgAgent, InstdDrctPty: p.InstdAgent,
		EndToEndID: p.EndToEndID, TxID: p.TxID, Amt: model.FormatFen(p.AmountFen), Ccy: p.Currency,
		SttlmDt: p.SettlementDate, BizTp: p.BizTp, BizKind: p.BizKind,
		DbtrNm: p.DebtorName, DbtrAcct: p.DebtorAcct, DbtrBk: p.DebtorBank,
		CdtrNm: p.CreditorName, CdtrAcct: p.CreditorAcct, CdtrBk: p.CreditorBank, Remark: p.Remark,
	}
}

// ---- pacs.002 <-> ibps.102 -----------------------------------------------------------------------

// ReceiptMsgID derives the ibps.102 MsgId from the payment key so it is identical on every retry and replay.
func ReceiptMsgID(p model.Payment) string {
	h := sha256.Sum256([]byte(p.PostingRef() + "|ibps.102"))
	return "R" + hex.EncodeToString(h[:])[:30]
}

// BuildPacs002 builds the status report for a decision.
func BuildPacs002(p model.Payment, o model.Outcome, msgID string) Pacs002 {
	var d Pacs002
	d.GrpHdr.MsgID = msgID
	d.GrpHdr.CreDtTm = o.DecidedAt.In(CST).Format(time.RFC3339)
	d.Orgnl.MsgID = p.MsgID
	d.Orgnl.MsgNmID = OrigMsgTypeIbps101
	d.Tx.OrgnlEndToEndID = p.EndToEndID
	if o.Accepted {
		d.Tx.TxSts = "ACCP"
	} else {
		d.Tx.TxSts = "RJCT"
		d.Tx.RsnCd = o.Reason.ISOCode()
	}
	d.Tx.AccptncDtTm = o.DecidedAt.In(CST).Format(time.RFC3339)
	return d
}

// Pacs002ToIBPS102 maps the status report to the synthetic receipt.
func (m SyntheticMapper) Pacs002ToIBPS102(d Pacs002) IBPS102 {
	r := IBPS102{
		MsgID: d.GrpHdr.MsgID, OrgnlMsgID: d.Orgnl.MsgID, OrgnlMsgTp: d.Orgnl.MsgNmID,
		OrgnlEndToEndID: d.Tx.OrgnlEndToEndID, PrcDtTm: d.Tx.AccptncDtTm,
	}
	if d.Tx.TxSts == "ACCP" {
		r.PrcSts = "accepted"
	} else {
		r.PrcSts = "rejected"
		r.RjctCd = d.Tx.RsnCd
		if v, ok := m.RejectCodes[d.Tx.RsnCd]; ok {
			r.RjctCd = v
		}
	}
	return r
}

// IBPS102ToPacs002 is the reverse mapping (simulator side).
func (m SyntheticMapper) IBPS102ToPacs002(r IBPS102) Pacs002 {
	var d Pacs002
	d.GrpHdr.MsgID = r.MsgID
	d.GrpHdr.CreDtTm = r.PrcDtTm
	d.Orgnl.MsgID = r.OrgnlMsgID
	d.Orgnl.MsgNmID = r.OrgnlMsgTp
	d.Tx.OrgnlEndToEndID = r.OrgnlEndToEndID
	d.Tx.AccptncDtTm = r.PrcDtTm
	if r.PrcSts == "accepted" {
		d.Tx.TxSts = "ACCP"
		return d
	}
	d.Tx.TxSts = "RJCT"
	d.Tx.RsnCd = r.RjctCd
	for iso, prop := range m.RejectCodes {
		if prop == r.RjctCd {
			d.Tx.RsnCd = iso
		}
	}
	return d
}

// ---- MessageMapper implementation -----------------------------------------------------------------

func marshal(v any) ([]byte, error) {
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}

func (m SyntheticMapper) InboundToPayment(raw []byte) (model.Payment, error) {
	var in IBPS101
	if err := xml.Unmarshal(raw, &in); err != nil {
		return model.Payment{}, &FormatError{err.Error()}
	}
	return Pacs008ToPayment(IBPS101ToPacs008(in))
}

func (m SyntheticMapper) OutboundReceipt(p model.Payment, o model.Outcome) ([]byte, string, error) {
	if !o.Accepted && o.Reason.ISOCode() == "" {
		return nil, "", errors.New("reject without a known reason: " + string(o.Reason))
	}
	id := ReceiptMsgID(p)
	raw, err := marshal(m.Pacs002ToIBPS102(BuildPacs002(p, o, id)))
	return raw, id, err
}

func (m SyntheticMapper) BuildInbound(p model.Payment) ([]byte, error) {
	return marshal(paymentToIBPS101(p))
}

func (m SyntheticMapper) ParseReceipt(raw []byte) (Receipt, error) {
	var r IBPS102
	if err := xml.Unmarshal(raw, &r); err != nil {
		return Receipt{}, err
	}
	d := m.IBPS102ToPacs002(r)
	at, err := time.Parse(time.RFC3339, r.PrcDtTm)
	if err != nil {
		return Receipt{}, fmt.Errorf("PrcDtTm: %w", err)
	}
	return Receipt{MsgID: r.MsgID, OrigMsgID: r.OrgnlMsgID, OrigEndToEndID: r.OrgnlEndToEndID,
		Accepted: r.PrcSts == "accepted", IsoReason: d.Tx.RsnCd, PrcDtTm: at}, nil
}

// MarshalPacs008 and MarshalPacs002 render the internal messages (golden tests, debugging).
func MarshalPacs008(d Pacs008) ([]byte, error) { return marshal(d) }
func MarshalPacs002(d Pacs002) ([]byte, error) { return marshal(d) }
