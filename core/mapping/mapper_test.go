package mapping

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bill/ibps-bench/core/model"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update): %v", name, err)
	}
	if string(want) != string(got) {
		t.Errorf("golden %s mismatch\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func samplePayment() model.Payment {
	return model.Payment{
		MsgID: "M20260928000001", EndToEndID: "E20260928000001", TxID: "T20260928000001",
		CreDtTm:    time.Date(2026, 9, 28, 10, 0, 0, 0, CST),
		InstgAgent: "102100099996", InstdAgent: "313000000000",
		AmountFen: 123456, Currency: "CNY", SettlementDate: "2026-09-28",
		BizTp: "A100", BizKind: "02102",
		DebtorName: "ZHANG SAN", DebtorAcct: "6222000000000001", DebtorBank: "102100099996",
		CreditorName: "CUSTOMER 00000042", CreditorAcct: "6222000000000042", CreditorBank: "313000000000",
		Remark: "invoice 42",
	}
}

var testMapper = SyntheticMapper{RejectCodes: map[string]string{"AC01": "PRTRY-AC01", "RR04": "PRTRY-RR04"}}

func TestGolden_IBPS101ToPacs008(t *testing.T) {
	raw, err := testMapper.BuildInbound(samplePayment())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ibps101.xml", raw)
	in := IBPS101{}
	if err := unmarshalForTest(raw, &in); err != nil {
		t.Fatal(err)
	}
	out, err := MarshalPacs008(IBPS101ToPacs008(in))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pacs008.xml", out)
}

func TestGolden_Pacs008ToIBPS101(t *testing.T) {
	p := samplePayment()
	got, err := marshal(Pacs008ToIBPS101(PaymentToPacs008(p)))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := testMapper.BuildInbound(p)
	if string(got) != string(want) {
		t.Errorf("pacs.008 -> ibps.101 not the inverse of ibps.101 -> pacs.008:\n%s\n%s", got, want)
	}
}

func TestGolden_OutboundReceipts(t *testing.T) {
	p := samplePayment()
	at := time.Date(2026, 9, 28, 10, 0, 1, 0, CST)
	acc, id, err := testMapper.OutboundReceipt(p, model.Outcome{Accepted: true, DecidedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ibps102_accepted.xml", acc)
	rj, id2, err := testMapper.OutboundReceipt(p, model.Outcome{Reason: model.ReasonAccountNotFound, DecidedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ibps102_rejected.xml", rj)
	if id != id2 || id != ReceiptMsgID(p) || !strings.HasPrefix(id, "R") || len(id) != 31 {
		t.Errorf("receipt id must be deterministic and 31 chars: %q %q", id, id2)
	}
	pacs, err := MarshalPacs002(BuildPacs002(p, model.Outcome{Reason: model.ReasonAMLHit, DecidedAt: at}, "R1"))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pacs002_rejected.xml", pacs)
}

func TestGolden_IBPS102ToPacs002(t *testing.T) {
	raw, err := os.ReadFile("testdata/ibps102_rejected.xml")
	if err != nil {
		t.Skip("golden not generated yet")
	}
	var r IBPS102
	if err := unmarshalForTest(raw, &r); err != nil {
		t.Fatal(err)
	}
	out, err := MarshalPacs002(testMapper.IBPS102ToPacs002(r))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pacs002_from_ibps102.xml", out)
}

func TestInboundRoundTrip(t *testing.T) {
	p := samplePayment()
	raw, _ := testMapper.BuildInbound(p)
	got, err := testMapper.InboundToPayment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreDtTm.Equal(p.CreDtTm) {
		t.Errorf("time changed: %v vs %v", got.CreDtTm, p.CreDtTm)
	}
	got.CreDtTm = p.CreDtTm
	if got != p {
		t.Errorf("round trip changed the payment:\n got %+v\nwant %+v", got, p)
	}
}

func TestReceiptRoundTrip(t *testing.T) {
	p := samplePayment()
	at := time.Date(2026, 9, 28, 10, 0, 1, 0, CST)
	cases := []model.Outcome{
		{Accepted: true, DecidedAt: at},
		{Reason: model.ReasonAccountNotFound, DecidedAt: at}, // proprietary mapped
		{Reason: model.ReasonNameMismatch, DecidedAt: at},    // ISO passthrough
	}
	for _, o := range cases {
		raw, _, err := testMapper.OutboundReceipt(p, o)
		if err != nil {
			t.Fatal(err)
		}
		r, err := testMapper.ParseReceipt(raw)
		if err != nil {
			t.Fatal(err)
		}
		if r.Accepted != o.Accepted || r.IsoReason != o.Reason.ISOCode() || r.OrigMsgID != p.MsgID ||
			r.OrigEndToEndID != p.EndToEndID || !r.PrcDtTm.Equal(at) || r.MsgID != ReceiptMsgID(p) {
			t.Errorf("receipt mismatch for %+v: %+v", o, r)
		}
	}
	if _, err := testMapper.ParseReceipt([]byte("<x")); err == nil {
		t.Error("garbage receipt must fail")
	}
	bad := strings.Replace(string(mustReceipt(t, p, at)), "2026-09-28T10:00:01+08:00", "yesterday", 2)
	if _, err := testMapper.ParseReceipt([]byte(bad)); err == nil {
		t.Error("bad PrcDtTm must fail")
	}
}

func mustReceipt(t *testing.T, p model.Payment, at time.Time) []byte {
	raw, _, err := testMapper.OutboundReceipt(p, model.Outcome{Accepted: true, DecidedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOutboundRejectWithoutReason(t *testing.T) {
	if _, _, err := testMapper.OutboundReceipt(samplePayment(), model.Outcome{Reason: "BOGUS"}); err == nil {
		t.Error("unknown reason must not build a receipt")
	}
}

func TestRemarkTruncatedAt140Runes(t *testing.T) {
	p := samplePayment()
	p.Remark = strings.Repeat("汉", 200)
	raw, _ := testMapper.BuildInbound(p)
	got, err := testMapper.InboundToPayment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got.Remark)); n != 140 {
		t.Errorf("remark has %d runes, want 140", n)
	}
	p.Remark = "short"
	raw, _ = testMapper.BuildInbound(p)
	if got, _ = testMapper.InboundToPayment(raw); got.Remark != "short" {
		t.Error("short remark must be untouched")
	}
}

func TestFixedPacs008Fields(t *testing.T) {
	d := PaymentToPacs008(samplePayment())
	if d.GrpHdr.NbOfTxs != 1 || d.GrpHdr.SttlmInf.SttlmMtd != "CLRG" || d.GrpHdr.SttlmInf.ClrSys != "IBPS" || d.Tx.ChrgBr != "SLEV" {
		t.Errorf("fixed fields wrong: %+v", d.GrpHdr)
	}
}

func TestParseFen(t *testing.T) {
	ok := map[string]int64{"0": 0, "1": 100, "1.5": 150, "1.05": 105, "123456.78": 12345678, "  9.99 ": 999}
	for in, want := range ok {
		got, err := ParseFen(in)
		if err != nil || got != want {
			t.Errorf("ParseFen(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "+1", ".5", "1.", "1.234", "abc", "1.a", "99999999999999999999"} {
		_, err := ParseFen(in)
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Errorf("ParseFen(%q) should be a FormatError, got %v", in, err)
		}
	}
}

func TestInboundFormatErrors(t *testing.T) {
	good, _ := testMapper.BuildInbound(samplePayment())
	s := string(good)
	mut := map[string]string{
		"garbage":      "<not-xml",
		"wrong root":   strings.Replace(strings.Replace(s, "<Ibps101", "<Other", 1), "</Ibps101>", "</Other>", 1),
		"missing msg":  strings.Replace(s, "<MsgId>M20260928000001</MsgId>", "<MsgId></MsgId>", 1),
		"long id":      strings.Replace(s, "M20260928000001", strings.Repeat("M", 36), 1),
		"bad time":     strings.Replace(s, "2026-09-28T10:00:00+08:00", "soon", 1),
		"bad amount":   strings.Replace(s, "1234.56", "12.345", 1),
		"missing ccy":  strings.Replace(s, "<Ccy>CNY</Ccy>", "<Ccy></Ccy>", 1),
		"missing cdtr": strings.Replace(s, "<CdtrAcct>6222000000000042</CdtrAcct>", "<CdtrAcct></CdtrAcct>", 1),
	}
	for name, in := range mut {
		_, err := testMapper.InboundToPayment([]byte(in))
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Errorf("%s: want FormatError, got %v", name, err)
		}
	}
	if (&FormatError{"x"}).Error() != "format invalid: x" {
		t.Error("FormatError text changed")
	}
}

func TestMarshalError(t *testing.T) {
	if _, err := marshal(make(chan int)); err == nil {
		t.Error("unsupported type must fail to marshal")
	}
}
