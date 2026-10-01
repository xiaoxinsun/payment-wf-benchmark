// Package steps is the shared business core: plain functions with no engine imports. Each engine binding
// wraps these as activities (Temporal) or steps (DBOS). Business outcomes are returned as values, never
// as errors, so an engine retries only transient failures and never a business reject.
package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bill/ibps-bench/core/config"
	"github.com/bill/ibps-bench/core/mapping"
	"github.com/bill/ibps-bench/core/model"
	"github.com/bill/ibps-bench/core/obs"
)

// Deps carries everything a step needs. Safe for concurrent use.
type Deps struct {
	Cfg    config.Config
	EP     config.Endpoints
	HTTP   *http.Client
	Mapper mapping.MessageMapper
	Store  *Store
}

// NewHTTPClient returns a client tuned identically for every variant. Per-request deadlines come from contexts.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxIdleConns: 1024, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second,
		ForceAttemptHTTP2: false,
	}}
}

// ErrTransient wraps failures an engine should retry (network, timeout, 5xx).
var ErrTransient = errors.New("transient failure")

func transient(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrTransient, fmt.Sprintf(format, a...))
}

// ---- step 2: pure checks ------------------------------------------------------------------------

// Check runs the step 2 business checks. It performs no I/O and is safe inside a Temporal workflow body.
func Check(sla config.SLA, p model.Payment) (model.RejectReason, bool) {
	switch {
	case p.InstdAgent != sla.OurBankCode:
		return model.ReasonWrongReceiver, true
	case p.Currency != "CNY":
		return model.ReasonCurrencyNotAllow, true
	case p.AmountFen <= 0:
		return model.ReasonFormatInvalid, true
	case p.AmountFen > sla.AmountLimitFen:
		return model.ReasonAmountOverLimit, true
	}
	return "", false
}

// ---- results -------------------------------------------------------------------------------------

type ValidateResult struct {
	Reject    model.RejectReason `json:"reject,omitempty"`
	DecidedAt time.Time          `json:"decidedAt"`
}

type ScreenResult struct {
	Verdict   string             `json:"verdict"` // CLEAR | REVIEW | REJECT
	CaseID    string             `json:"caseId,omitempty"`
	Reject    model.RejectReason `json:"reject,omitempty"`
	DecidedAt time.Time          `json:"decidedAt"`
}

type ReviewResult struct {
	Reject    model.RejectReason `json:"reject,omitempty"`
	DecidedAt time.Time          `json:"decidedAt"`
}

type PostResult struct {
	PostedAt      time.Time `json:"postedAt"`
	AlreadyPosted bool      `json:"alreadyPosted"`
}

func (d *Deps) attemptCtx(ctx context.Context, timeout time.Duration, deadline time.Time) (context.Context, context.CancelFunc) {
	if !deadline.IsZero() {
		if until := time.Until(deadline); until < timeout {
			timeout = until
		}
	}
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	return context.WithTimeout(ctx, timeout)
}

func (d *Deps) do(ctx context.Context, method, u string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func slaExpired(in model.PaymentInput) bool {
	return !in.Deadline.IsZero() && time.Now().After(in.Deadline)
}

// ---- step 3: validate account --------------------------------------------------------------------

var validateReasons = map[string]model.RejectReason{
	"ACCOUNT_NOT_FOUND": model.ReasonAccountNotFound,
	"CLOSED":            model.ReasonAccountClosed,
	"FROZEN":            model.ReasonAccountFrozen,
	"DORMANT":           model.ReasonAccountDormant,
	"NAME_MISMATCH":     model.ReasonNameMismatch,
}

// ValidateAccount is one attempt of step 3. Business rejects come back as a result; only transient
// failures are errors.
func (d *Deps) ValidateAccount(ctx context.Context, in model.PaymentInput) (ValidateResult, error) {
	defer obs.ObserveStep("validate", time.Now())
	if slaExpired(in) {
		return ValidateResult{Reject: model.ReasonSLATimeout, DecidedAt: time.Now()}, nil
	}
	p := in.Payment
	ctx, cancel := d.attemptCtx(ctx, d.Cfg.Timeouts.Step3Validate, in.Deadline)
	defer cancel()
	u := fmt.Sprintf("%s/accounts/%s/validate?name=%s&ccy=%s", d.EP.CBSURL, url.PathEscape(p.CreditorAcct), url.QueryEscape(p.CreditorName), url.QueryEscape(p.Currency))
	code, body, err := d.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ValidateResult{}, transient("validate: %v", err)
	}
	switch {
	case code == http.StatusOK:
		return ValidateResult{DecidedAt: time.Now()}, nil
	case code == http.StatusUnprocessableEntity:
		var r struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(body, &r)
		if reason, ok := validateReasons[r.Reason]; ok {
			return ValidateResult{Reject: reason, DecidedAt: time.Now()}, nil
		}
		return ValidateResult{}, transient("validate: unknown 422 reason %q", r.Reason)
	}
	return ValidateResult{}, transient("validate: HTTP %d", code)
}

// ---- step 4: AML ---------------------------------------------------------------------------------

// ScreenAML is one attempt of step 4.
func (d *Deps) ScreenAML(ctx context.Context, in model.PaymentInput) (ScreenResult, error) {
	defer obs.ObserveStep("screen", time.Now())
	if slaExpired(in) {
		return ScreenResult{Verdict: "REJECT", Reject: model.ReasonSLATimeout, DecidedAt: time.Now()}, nil
	}
	p := in.Payment
	ctx, cancel := d.attemptCtx(ctx, d.Cfg.Timeouts.Step4Screen, in.Deadline)
	defer cancel()
	code, body, err := d.do(ctx, http.MethodPost, d.EP.AMLURL+"/screen", map[string]string{
		"msgId": p.MsgID, "debtorName": p.DebtorName, "creditorName": p.CreditorName})
	if err != nil {
		return ScreenResult{}, transient("screen: %v", err)
	}
	if code != http.StatusOK {
		return ScreenResult{}, transient("screen: HTTP %d", code)
	}
	var r struct {
		Result string `json:"result"`
		CaseID string `json:"caseId"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ScreenResult{}, transient("screen: bad body: %v", err)
	}
	now := time.Now()
	switch r.Result {
	case "CLEAR":
		if slaExpired(in) { // last pre-settlement decision point: the SLA is enforced here, durably
			return ScreenResult{Verdict: "REJECT", Reject: model.ReasonSLATimeout, DecidedAt: now}, nil
		}
		return ScreenResult{Verdict: "CLEAR", DecidedAt: now}, nil
	case "HIT":
		return ScreenResult{Verdict: "REJECT", Reject: model.ReasonAMLHit, DecidedAt: now}, nil
	case "REVIEW":
		return ScreenResult{Verdict: "REVIEW", CaseID: r.CaseID, DecidedAt: now}, nil
	}
	return ScreenResult{}, transient("screen: unknown result %q", r.Result)
}

// AwaitReview (step 4b) polls the case until it resolves or the SLA budget is spent. Poll errors are
// treated as "still open" until the deadline.
func (d *Deps) AwaitReview(ctx context.Context, in model.PaymentInput, caseID string) (ReviewResult, error) {
	defer obs.ObserveStep("review", time.Now())
	every := d.Cfg.Retries.Step4bReview.PollEvery
	if every <= 0 {
		every = 250 * time.Millisecond
	}
	for {
		if slaExpired(in) {
			return ReviewResult{Reject: model.ReasonAMLReviewTimeout, DecidedAt: time.Now()}, nil
		}
		pctx, cancel := d.attemptCtx(ctx, d.Cfg.Timeouts.Step4bReviewPoll, in.Deadline)
		code, body, err := d.do(pctx, http.MethodGet, d.EP.AMLURL+"/cases/"+url.PathEscape(caseID), nil)
		cancel()
		if err == nil && code == http.StatusOK {
			var r struct {
				State string `json:"state"`
			}
			if json.Unmarshal(body, &r) == nil {
				switch r.State {
				case "CLEARED":
					if slaExpired(in) {
						return ReviewResult{Reject: model.ReasonSLATimeout, DecidedAt: time.Now()}, nil
					}
					return ReviewResult{DecidedAt: time.Now()}, nil
				case "CONFIRMED_HIT":
					return ReviewResult{Reject: model.ReasonAMLHit, DecidedAt: time.Now()}, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ReviewResult{}, transient("review: %v", ctx.Err())
		case <-time.After(every):
		}
	}
}

// ---- step 5: settlement --------------------------------------------------------------------------

// SettlementAccount picks the settlement sub-account for a posting reference.
func SettlementAccount(postingRef string, shards int) string {
	h := fnv.New32a()
	h.Write([]byte(postingRef))
	return fmt.Sprintf("SETTLE-IBPS-%d", int(h.Sum32())%shards)
}

type postingLeg struct {
	Account   string `json:"account"`
	Side      string `json:"side"` // D or C
	AmountFen int64  `json:"amountFen"`
}

type postingBody struct {
	PostingRef string       `json:"postingRef"`
	Legs       []postingLeg `json:"legs"`
}

type postingResp struct {
	Status   string    `json:"status"`
	PostedAt time.Time `json:"postedAt"`
}

// PostSettlement is one attempt of step 5. On any ambiguous failure it asks the CBS whether the posting
// exists before reporting a transient error, so a retry with the same reference can never double-post.
func (d *Deps) PostSettlement(ctx context.Context, in model.PaymentInput) (PostResult, error) {
	defer obs.ObserveStep("post", time.Now())
	p := in.Payment
	ref := p.PostingRef()
	body := postingBody{PostingRef: ref, Legs: []postingLeg{
		{Account: SettlementAccount(ref, d.Cfg.SLA.SettlementShards), Side: "D", AmountFen: p.AmountFen},
		{Account: p.CreditorAcct, Side: "C", AmountFen: p.AmountFen},
	}}
	pctx, cancel := context.WithTimeout(ctx, d.Cfg.Timeouts.Step5Post)
	code, raw, err := d.do(pctx, http.MethodPost, d.EP.CBSURL+"/postings", body)
	cancel()
	if err == nil && (code == http.StatusCreated || code == http.StatusOK) {
		var r postingResp
		_ = json.Unmarshal(raw, &r)
		return PostResult{PostedAt: r.PostedAt, AlreadyPosted: code == http.StatusOK}, nil
	}
	if err == nil && code == http.StatusConflict {
		return PostResult{}, fmt.Errorf("CBS refused an unbalanced posting for %s (bug guard): %s", ref, raw)
	}
	// Ambiguous (timeout, dropped response, 5xx): resolve by status query before any retry.
	return d.resolvePosting(ctx, ref, err, code)
}

func (d *Deps) resolvePosting(ctx context.Context, ref string, postErr error, postCode int) (PostResult, error) {
	qctx, cancel := context.WithTimeout(ctx, d.Cfg.Timeouts.Step5StatusQuery)
	defer cancel()
	code, raw, err := d.do(qctx, http.MethodGet, d.EP.CBSURL+"/postings/"+url.PathEscape(ref), nil)
	switch {
	case err != nil:
		return PostResult{}, transient("post %s ambiguous (post: %v/%d) and status query failed: %v", ref, postErr, postCode, err)
	case code == http.StatusOK:
		var r postingResp
		_ = json.Unmarshal(raw, &r)
		return PostResult{PostedAt: r.PostedAt, AlreadyPosted: true}, nil
	case code == http.StatusNotFound:
		return PostResult{}, transient("post %s not found after ambiguous outcome (post: %v/%d); retry same reference", ref, postErr, postCode)
	}
	return PostResult{}, transient("post %s status query HTTP %d", ref, code)
}

// ---- step 6: respond -----------------------------------------------------------------------------

// SendReceipt is one attempt of step 6. The ibps.102 MsgId is deterministic, so retries and replays
// deliver the same logical receipt.
func (d *Deps) SendReceipt(ctx context.Context, in model.PaymentInput, o model.Outcome) error {
	defer obs.ObserveStep("respond", time.Now())
	raw, _, err := d.Mapper.OutboundReceipt(in.Payment, o)
	if err != nil {
		return fmt.Errorf("build receipt: %w", err) // programming error, not transient
	}
	ctx, cancel := context.WithTimeout(ctx, d.Cfg.Timeouts.Step6Respond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.EP.NPCURL+"/npc/ibps102", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return transient("respond: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return transient("respond: HTTP %d", resp.StatusCode)
	}
	return nil
}

// CodeFault reports whether the C1 probe should panic the workflow body for this payment. It is inert
// unless CODE_FAULT_SUFFIX is set, and exists only to observe how each engine surfaces a stuck workflow.
func CodeFault(msgID string) bool {
	suffix := codeFaultSuffix
	return suffix != "" && strings.HasSuffix(msgID, suffix)
}

var codeFaultSuffix = os.Getenv("CODE_FAULT_SUFFIX")
