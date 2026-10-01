// Package mapping converts between the synthetic IBPS messages (edge only) and the pacs messages the
// platform speaks internally. The bank's real IBPS spec replaces SyntheticMapper without touching workflows.
package mapping

import "encoding/xml"

const (
	NSIbps101 = "urn:cncc:ibps:synthetic:ibps.101.001.01"
	NSIbps102 = "urn:cncc:ibps:synthetic:ibps.102.001.01"
	NSPacs008 = "urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08"
	NSPacs002 = "urn:iso:std:iso:20022:tech:xsd:pacs.002.001.10"

	OrigMsgTypeIbps101 = "ibps.101.001.01"
)

// IBPS101 is the simplified synthetic inward customer credit transfer.
type IBPS101 struct {
	XMLName      xml.Name `xml:"urn:cncc:ibps:synthetic:ibps.101.001.01 Ibps101"`
	MsgID        string   `xml:"MsgId"`
	CreDtTm      string   `xml:"CreDtTm"`
	InstgDrctPty string   `xml:"InstgDrctPty"`
	InstdDrctPty string   `xml:"InstdDrctPty"`
	EndToEndID   string   `xml:"EndToEndId"`
	TxID         string   `xml:"TxId"`
	Amt          string   `xml:"Amt"`
	Ccy          string   `xml:"Ccy"`
	SttlmDt      string   `xml:"SttlmDt"`
	BizTp        string   `xml:"BizTp"`
	BizKind      string   `xml:"BizKind"`
	DbtrNm       string   `xml:"DbtrNm"`
	DbtrAcct     string   `xml:"DbtrAcct"`
	DbtrBk       string   `xml:"DbtrBk"`
	CdtrNm       string   `xml:"CdtrNm"`
	CdtrAcct     string   `xml:"CdtrAcct"`
	CdtrBk       string   `xml:"CdtrBk"`
	Remark       string   `xml:"Remark"`
}

// IBPS102 is the simplified synthetic receipt.
type IBPS102 struct {
	XMLName         xml.Name `xml:"urn:cncc:ibps:synthetic:ibps.102.001.01 Ibps102"`
	MsgID           string   `xml:"MsgId"`
	OrgnlMsgID      string   `xml:"OrgnlMsgId"`
	OrgnlMsgTp      string   `xml:"OrgnlMsgTp"`
	OrgnlEndToEndID string   `xml:"OrgnlEndToEndId"`
	PrcSts          string   `xml:"PrcSts"`
	RjctCd          string   `xml:"RjctCd,omitempty"`
	PrcDtTm         string   `xml:"PrcDtTm"`
}

type clrSysMmbID struct {
	MmbID string `xml:"FinInstnId>ClrSysMmbId>MmbId"`
}

type acctID struct {
	ID string `xml:"Id>Othr>Id"`
}

type party struct {
	Nm string `xml:"Nm"`
}

type amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// Pacs008 is the subset of pacs.008.001.08 (FIToFICstmrCdtTrf, one transaction) used by the platform.
type Pacs008 struct {
	XMLName xml.Name `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08 Document"`
	GrpHdr  struct {
		MsgID    string `xml:"MsgId"`
		CreDtTm  string `xml:"CreDtTm"`
		NbOfTxs  int    `xml:"NbOfTxs"`
		SttlmInf struct {
			SttlmMtd string `xml:"SttlmMtd"`
			ClrSys   string `xml:"ClrSys>Prtry"`
		} `xml:"SttlmInf"`
		InstgAgt clrSysMmbID `xml:"InstgAgt"`
		InstdAgt clrSysMmbID `xml:"InstdAgt"`
	} `xml:"FIToFICstmrCdtTrf>GrpHdr"`
	Tx struct {
		EndToEndID string      `xml:"PmtId>EndToEndId"`
		TxID       string      `xml:"PmtId>TxId"`
		CtgyPurp   string      `xml:"PmtTpInf>CtgyPurp>Prtry"`
		Amt        amount      `xml:"IntrBkSttlmAmt"`
		SttlmDt    string      `xml:"IntrBkSttlmDt"`
		ChrgBr     string      `xml:"ChrgBr"`
		Dbtr       party       `xml:"Dbtr"`
		DbtrAcct   acctID      `xml:"DbtrAcct"`
		DbtrAgt    clrSysMmbID `xml:"DbtrAgt"`
		CdtrAgt    clrSysMmbID `xml:"CdtrAgt"`
		Cdtr       party       `xml:"Cdtr"`
		CdtrAcct   acctID      `xml:"CdtrAcct"`
		Purp       string      `xml:"Purp>Prtry"`
		Ustrd      string      `xml:"RmtInf>Ustrd"`
	} `xml:"FIToFICstmrCdtTrf>CdtTrfTxInf"`
}

// Pacs002 is the subset of pacs.002.001.10 (FIToFIPmtStsRpt, one transaction) used by the platform.
type Pacs002 struct {
	XMLName xml.Name `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.002.001.10 Document"`
	GrpHdr  struct {
		MsgID   string `xml:"MsgId"`
		CreDtTm string `xml:"CreDtTm"`
	} `xml:"FIToFIPmtStsRpt>GrpHdr"`
	Orgnl struct {
		MsgID   string `xml:"OrgnlMsgId"`
		MsgNmID string `xml:"OrgnlMsgNmId"`
	} `xml:"FIToFIPmtStsRpt>OrgnlGrpInfAndSts"`
	Tx struct {
		OrgnlEndToEndID string `xml:"OrgnlEndToEndId"`
		TxSts           string `xml:"TxSts"`
		RsnCd           string `xml:"StsRsnInf>Rsn>Cd,omitempty"`
		AccptncDtTm     string `xml:"AccptncDtTm"`
	} `xml:"FIToFIPmtStsRpt>TxInfAndSts"`
}
