package ccbill

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// #696: DataLink Subscription Management System — per-subscription read
// (viewSubscriptionStatus) + merchant-initiated cancel (cancelSubscription)
// via POST {BaseURL}/utils/subscriptionManagement.cgi.
//
// WIRE SHAPE VERIFIED against the live production DataLink account 2026-07-03
// (#696 Phase 0). Confirmed by real viewSubscriptionStatus/cancelSubscription
// round-trips: endpoint + form params below; response is
// <?xml version='1.0'?><results>…</results>. cancelSubscription success is a
// bare <results>1</results>; auth/authz rejection is HTTP 200 + a NEGATIVE
// bare results code (-7 observed = collapsed auth class). A status read returns
// a document of leaf fields — the real ones seen: subscriptionStatus,
// recurringSubscription, nextBillingDate, expirationDate, cancelDate,
// signupDate, timesRebilled, refundsIssued/returnsIssued/voidsIssued/
// chargebacksIssued. Dates are 8-digit YYYYMMDD (nextBillingDate) or 14-digit
// YYYYMMDDHHMMSS (expirationDate/signupDate/cancelDate).

const subscriptionManagementPath = "/utils/subscriptionManagement.cgi"

// SMS actions.
const (
	actionViewSubscriptionStatus = "viewSubscriptionStatus"
	actionCancelSubscription     = "cancelSubscription"
	// actionVoidOrRefundTransaction voids an unsettled transaction, else refunds
	// a settled one — one action covers both states (#696 refund leg). WIRE
	// PROVISIONAL (see RefundTransaction).
	actionVoidOrRefundTransaction = "voidOrRefundTransaction"
)

// ErrProviderReadOnly is returned by every CCBill mutation when the provider
// is read-only (mode=readonly, #346) — blocked before any bytes hit the wire,
// mirroring nmi.ErrProviderReadOnly.
var ErrProviderReadOnly = errors.New("ccbill: provider writes are blocked (mode=readonly)")

// ErrDataLinkAuth marks a DataLink DENIAL — the request was received but NOT
// executed. It is CCBill's OVERLOADED refusal signal: HTTP 401/403, an
// auth-shaped error body, OR a bare results=-7. -7 is returned BOTH for a wrong
// password (Phase 0) AND (VERIFIED 2026-07-03, safe fail-probe, no money moved)
// for an operation not permitted on the target — a refund/void of a too-old,
// non-refundable transaction with VALID creds — the two are indistinguishable
// from outside. So a -7 may be a transient auth/IP flap OR a permanent
// "not permitted": callers must bounded-retry then go terminal, NOT clean-retry
// forever as if it were pure auth. (Symbol name kept for continuity; read it as
// "request denied", not "authentication rejected".)
var ErrDataLinkAuth = errors.New("ccbill datalink: request denied (auth or operation not permitted)")

// ErrCancelRejected marks a received, parsed, DEFINITE non-success answer to
// cancelSubscription (results != 1). The verbatim provider answer is wrapped.
// Callers on the intents path verify-not-decline: a reject may mean
// already-cancelled — the viewSubscriptionStatus re-read decides.
var ErrCancelRejected = errors.New("ccbill datalink: cancelSubscription rejected")

// ErrRefundRejected marks a received, parsed, DEFINITE non-success answer to the
// refund action (results != 1, not the -7 auth class). The verbatim answer is
// wrapped. Callers verify-not-decline: a reject may mean already-refunded — the
// viewSubscriptionStatus counter re-read decides.
var ErrRefundRejected = errors.New("ccbill datalink: refund rejected")

// maxSubscriptionManagementResponseBytes bounds one SMS response (tiny XML).
const maxSubscriptionManagementResponseBytes = 1 << 20

// subscriptionStatusExpiryFields are the response fields consulted (in order)
// for the paid-through / expiry date. VERIFIED (#696 Phase 0): an ACTIVE sub
// carries the forward-looking nextBillingDate (next charge = paid-through); a
// dead sub carries expirationDate (when access ended). nextBillingDate wins
// when present.
var subscriptionStatusExpiryFields = []string{"nextBillingDate", "expirationDate"}

// subscriptionStatusDateLayouts are the accepted expiry encodings. VERIFIED
// (#696 Phase 0): 14-digit YYYYMMDDHHMMSS (expirationDate/signupDate/cancelDate)
// and 8-digit YYYYMMDD (nextBillingDate).
var subscriptionStatusDateLayouts = []string{"20060102150405", "20060102"}

// SubscriptionStatusResult is one parsed viewSubscriptionStatus answer.
// RawStatus is the verbatim <subscriptionStatus> value (required — a response
// without it is an error, never a defaulted struct); Fields preserves every
// response leaf verbatim.
type SubscriptionStatusResult struct {
	RawStatus string
	Fields    map[string]string
}

// Rebilling reports whether CCBill will attempt future rebills. Status
// vocabulary VERIFIED against production (#696 Phase 0) for "2" (active
// recurring: nextBillingDate future, timesRebilled>0) and "0" (dead:
// expirationDate past); "1" (active non-recurring / cancelled-with-runway)
// unobserved but held from the DataLink docs.
//
//	"2" = active, recurring        -> will rebill
//	"1" = active, non-recurring    -> no rebill (cancelled-with-runway / one-time)
//	"0" = inactive / expired       -> no rebill
//
// NOTE: rebill prediction keys off subscriptionStatus, NOT the separate
// `recurringSubscription` field — a dead sub still reports recurringSubscription=1
// (it WAS a recurring plan) yet will never rebill. Unrecognized values are an
// ERROR (#651: never silently mapped).
func (r SubscriptionStatusResult) Rebilling() (bool, error) {
	switch strings.TrimSpace(r.RawStatus) {
	case "2":
		return true, nil
	case "1", "0":
		return false, nil
	default:
		return false, fmt.Errorf("ccbill datalink: unrecognized subscriptionStatus %q", r.RawStatus)
	}
}

// Active reports whether the subscription still grants access (statuses "2"
// and "1"). Same provisional vocabulary + unrecognized-is-error as Rebilling.
func (r SubscriptionStatusResult) Active() (bool, error) {
	switch strings.TrimSpace(r.RawStatus) {
	case "2", "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("ccbill datalink: unrecognized subscriptionStatus %q", r.RawStatus)
	}
}

// ExpiresAt returns the parsed expiry/paid-through instant when the response
// carried one (end-of-day UTC for date-only values, matching the webhook
// convention). ok=false when absent or unparseable — never a fabricated date.
func (r SubscriptionStatusResult) ExpiresAt() (time.Time, bool) {
	for _, field := range subscriptionStatusExpiryFields {
		raw := strings.TrimSpace(r.Fields[field])
		if raw == "" {
			continue
		}
		parsed, err := timeutil.ParseFirstUTC(raw, subscriptionStatusDateLayouts...)
		if err != nil {
			continue
		}
		if parsed.Hour() == 0 && parsed.Minute() == 0 && parsed.Second() == 0 {
			parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 23, 59, 59, 0, time.UTC)
		}
		return parsed, true
	}
	return time.Time{}, false
}

// RefundsAndVoidsIssued sums the per-subscription refundsIssued + voidsIssued
// counters and reports whether they were present. VERIFIED present on active-sub
// reads (#696 Phase 0: refundsIssued=0, voidsIssued=0). returnsIssued
// (chargebacks/reversals) is deliberately EXCLUDED — it is not a merchant refund.
// known=false when the response omits the primary refundsIssued counter (some
// dead-sub variants do); the caller then cannot conclude "no refund occurred".
// This is the ONLY refund signal CCBill's read surface exposes — there is no
// per-transaction refund action to attribute a refund to one charge (unlike NMI).
func (r SubscriptionStatusResult) RefundsAndVoidsIssued() (total int64, known bool) {
	refunds, rOK := subscriptionIntField(r.Fields, "refundsIssued")
	voids, _ := subscriptionIntField(r.Fields, "voidsIssued")
	return refunds + voids, rOK
}

func subscriptionIntField(fields map[string]string, name string) (int64, bool) {
	raw := strings.TrimSpace(fields[name])
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// CancelResult carries the verbatim provider answer to cancelSubscription.
type CancelResult struct {
	Results string // verbatim results token ("1" on success)
}

// RefundResult carries the verbatim provider answer to a refund action.
type RefundResult struct {
	Results string // verbatim results token ("1" on success)
	Action  string // the SMS action sent (voidOrRefundTransaction)
}

// ViewSubscriptionStatus reads ONE subscription's provider state — a READ,
// allowed under readonly. Every non-answer (transport, auth, error code,
// missing subscriptionStatus) is an error: the caller's row stays
// unknown/retried, never resolved off a guess. NOTE (Phase 0): the
// unknown-subscription response shape is uncaptured; until then it surfaces
// as an error, NOT as authoritative absence.
func (c *DataLinkClient) ViewSubscriptionStatus(ctx context.Context, subscriptionID string) (SubscriptionStatusResult, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return SubscriptionStatusResult{}, fmt.Errorf("ccbill datalink: subscription id is required")
	}
	body, err := c.postSubscriptionManagement(ctx, actionViewSubscriptionStatus, subscriptionID)
	if err != nil {
		return SubscriptionStatusResult{}, err
	}
	fields, bare, err := parseSubscriptionManagementBody(body)
	if err != nil {
		return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: %w", err)
	}
	if bare != "" {
		if err := classifyResultsCode(bare); err != nil {
			return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: %w", err)
		}
		return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: provider answered bare code %q instead of a status document", bare)
	}
	raw, ok := fields["subscriptionStatus"]
	if !ok || strings.TrimSpace(raw) == "" {
		if code, hasCode := fields["results"]; hasCode {
			if err := classifyResultsCode(code); err != nil {
				return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: %w", err)
			}
			return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: provider answered results=%q instead of a status document", code)
		}
		return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: response is missing subscriptionStatus")
	}
	return SubscriptionStatusResult{RawStatus: strings.TrimSpace(raw), Fields: fields}, nil
}

// CancelSubscription stops future rebills for ONE subscription (access runs
// through the paid period — CCBill semantics). A MUTATION: blocked with
// ErrProviderReadOnly before any HTTP under mode=readonly. Error classes:
//
//	ErrProviderReadOnly — not attempted (transport gate)
//	ErrDataLinkAuth     — provider DENIAL (-7: auth OR not permitted), not executed; caller bounded-retry→terminal
//	ErrCancelRejected   — provider answered a definite non-success (verify)
//	anything else       — the request MAY have executed (verify, never assume)
func (c *DataLinkClient) CancelSubscription(ctx context.Context, subscriptionID string) (CancelResult, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return CancelResult{}, fmt.Errorf("ccbill datalink: subscription id is required")
	}
	if c.ReadOnly {
		return CancelResult{}, ErrProviderReadOnly
	}
	body, err := c.postSubscriptionManagement(ctx, actionCancelSubscription, subscriptionID)
	if err != nil {
		return CancelResult{}, err
	}
	fields, bare, err := parseSubscriptionManagementBody(body)
	if err != nil {
		// Received bytes we cannot interpret — the cancel may have executed.
		return CancelResult{}, fmt.Errorf("ccbill cancelSubscription: unparseable response: %w", err)
	}
	results := bare
	if results == "" {
		results = strings.TrimSpace(fields["results"])
	}
	if results == "1" {
		return CancelResult{Results: results}, nil
	}
	// A NEGATIVE results code is an auth/authz rejection (verified: -7), NOT a
	// subscription-specific "cancel refused" — the request did not execute, so
	// it is a clean retry (ErrDataLinkAuth), never a verify-not-decline ambiguity.
	if err := classifyResultsCode(results); err != nil {
		return CancelResult{Results: results}, err
	}
	return CancelResult{Results: results}, fmt.Errorf("%w: results=%q", ErrCancelRejected, results)
}

// RefundTransaction refunds (or voids-then-refunds) ONE transaction of a
// subscription through the DataLink SMS choke point. A MUTATION: blocked with
// ErrProviderReadOnly before any HTTP under mode=readonly. amountCents > 0 sends
// a PARTIAL refund amount; 0 refunds the whole transaction (amount omitted).
//
// WIRE PROVISIONAL — the refund request+response shape is UNVERIFIED. It cannot
// be probed live (real money), so it is modeled on the same SMS pattern as
// cancel: action=voidOrRefundTransaction (handles settled + unsettled), keyed by
// subscriptionId, carrying the original transactionId for per-transaction
// precision and an optional DECIMAL amount for partials; the answer is parsed
// with the SAME success(=1)/auth(-7)/reject envelope as cancel. On the FIRST
// real refund, CONFIRM: (1) the success / partial / already-refunded result
// codes, (2) the amount format (decimal dollars — assumed here — vs cents),
// (3) whether transactionId narrows to that charge. Then pin a golden test.
//
// Error classes mirror CancelSubscription:
//
//	ErrProviderReadOnly — not attempted (transport gate)
//	ErrDataLinkAuth     — provider DENIAL (-7: auth OR not permitted), not executed; caller bounded-retry→terminal
//	ErrRefundRejected   — provider answered a definite non-success (verify)
//	anything else       — the request MAY have executed (verify, never assume)
func (c *DataLinkClient) RefundTransaction(ctx context.Context, subscriptionID, transactionID string, amountCents moneyutil.Cents) (RefundResult, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	transactionID = strings.TrimSpace(transactionID)
	if subscriptionID == "" {
		return RefundResult{}, fmt.Errorf("ccbill datalink: subscription id is required")
	}
	if amountCents < 0 {
		return RefundResult{}, fmt.Errorf("ccbill datalink: refund amount must not be negative")
	}
	if c.ReadOnly {
		return RefundResult{}, ErrProviderReadOnly
	}
	action := actionVoidOrRefundTransaction
	body, err := c.postSubscriptionManagementForm(ctx, action, c.refundForm(action, subscriptionID, transactionID, amountCents))
	if err != nil {
		return RefundResult{}, err
	}
	fields, bare, err := parseSubscriptionManagementBody(body)
	if err != nil {
		// Received bytes we cannot interpret — the refund may have executed.
		return RefundResult{}, fmt.Errorf("ccbill %s: unparseable response: %w", action, err)
	}
	results := bare
	if results == "" {
		results = strings.TrimSpace(fields["results"])
	}
	if results == "1" {
		return RefundResult{Results: results, Action: action}, nil
	}
	// A NEGATIVE results code is the auth/authz rejection class (verified: -7),
	// NOT a refund-specific "refused" — the request did not execute, so a clean
	// retry (ErrDataLinkAuth), never a verify-not-decline ambiguity.
	if err := classifyResultsCode(results); err != nil {
		return RefundResult{Results: results, Action: action}, err
	}
	return RefundResult{Results: results, Action: action}, fmt.Errorf("%w: results=%q", ErrRefundRejected, results)
}

// refundForm builds the refund SMS request form: the base subscription-management
// params + the original transactionId (precision — never let CCBill pick the
// latest charge) + an optional amount for a PARTIAL refund.
//
// WIRE PROVISIONAL — the amount is encoded as a DECIMAL major-unit string (e.g.
// "9.99"), matching every other CCBill money field (billedInitialPrice) and the
// classic FormatCentsDecimal wire; it is NOT integer cents. The dollars-vs-cents
// guess lives on ONE line so a first-real-refund correction is trivial; decimal
// is also the SAFER guess (if CCBill wanted cents, "9.99" under-refunds/errors,
// whereas "999" would over-refund 100x).
func (c *DataLinkClient) refundForm(action, subscriptionID, transactionID string, amountCents moneyutil.Cents) url.Values {
	form := c.subscriptionManagementForm(action, subscriptionID)
	if transactionID != "" {
		form.Set("transactionId", transactionID)
	}
	if amountCents > 0 {
		form.Set("amount", moneyutil.FormatCentsDecimal(int64(amountCents)))
	}
	return form
}

// dataLinkAuthResultsCode is the VERIFIED collapsed DENIAL code. -7 is
// OVERLOADED: bad creds / IP not whitelisted / subsystem not enabled ALL return
// -7 (Phase 0), AND (2026-07-03 refund fail-probe) a refund/void of a too-old,
// non-refundable transaction with VALID creds ALSO returns -7 — indistinguishable
// from outside. It signals "request denied" (auth OR operation-not-permitted),
// NOT auth specifically; the operation did NOT execute. Only this exact code is
// classified as a denial — other negative/error codes carry unverified meanings
// and must not be mapped (no silent fabrication).
const dataLinkAuthResultsCode = "-7"

// classifyResultsCode maps a DataLink bare <results> code to ErrDataLinkAuth iff
// it is the confirmed DENIAL code (-7); else nil (caller-specific handling). The
// request did NOT execute on -7, but -7 is OVERLOADED (auth OR
// operation-not-permitted) so callers bounded-retry then go terminal rather than
// clean-retrying forever.
func classifyResultsCode(code string) error {
	if strings.TrimSpace(strings.Trim(code, `"`)) == dataLinkAuthResultsCode {
		return fmt.Errorf("%w: results=%q", ErrDataLinkAuth, dataLinkAuthResultsCode)
	}
	return nil
}

// subscriptionManagementForm builds the SMS request form. ONE function owns
// the wire params so a Phase-0 correction is a one-line change. Provisional
// open question for Phase 0: whether the subaccount param is `clientSubacc`
// (tracker's best-known shape, used here) or `usingSubacc` (s2member).
func (c *DataLinkClient) subscriptionManagementForm(action, subscriptionID string) url.Values {
	form := url.Values{}
	form.Set("clientAccnum", c.ClientAccNum)
	if c.ClientSubAcc != "" {
		form.Set("clientSubacc", c.ClientSubAcc)
	}
	form.Set("username", c.Username)
	form.Set("password", c.Password)
	form.Set("action", action)
	form.Set("subscriptionId", subscriptionID)
	form.Set("returnXML", "1")
	return form
}

// postSubscriptionManagement performs ONE SMS round-trip for the simple
// (action, subscriptionId) requests (view + cancel), delegating the transport to
// postSubscriptionManagementForm.
func (c *DataLinkClient) postSubscriptionManagement(ctx context.Context, action, subscriptionID string) (string, error) {
	return c.postSubscriptionManagementForm(ctx, action, c.subscriptionManagementForm(action, subscriptionID))
}

// postSubscriptionManagementForm performs ONE SMS round-trip (no retry loop — the
// intents executor/verifier own retries; blind HTTP retries on a mutation
// endpoint are unsafe) and returns the raw trimmed body. Auth-shaped
// rejections (401/403, auth-error body) surface as ErrDataLinkAuth. ONE transport
// serves view/cancel/refund so a change is a single edit.
func (c *DataLinkClient) postSubscriptionManagementForm(ctx context.Context, action string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+subscriptionManagementPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("ccbill %s: build request: %w", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "OpenRails/1.0")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ccbill %s: %w", action, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionManagementResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("ccbill %s: read response: %w", action, err)
	}
	if len(raw) > maxSubscriptionManagementResponseBytes {
		return "", fmt.Errorf("ccbill %s: response exceeded %d bytes", action, maxSubscriptionManagementResponseBytes)
	}
	body := strings.TrimSpace(string(raw))

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w (http %d)", ErrDataLinkAuth, resp.StatusCode)
	case http.StatusOK:
		// fall through to body classification
	default:
		return "", fmt.Errorf("ccbill %s: unexpected http status %d", action, resp.StatusCode)
	}
	if isDataLinkAuthErrorBody(body) {
		return "", fmt.Errorf("%w: %s", ErrDataLinkAuth, truncateForError(body))
	}
	if body == "" {
		return "", fmt.Errorf("ccbill %s: empty response body", action)
	}
	return body, nil
}

// isDataLinkAuthErrorBody sniffs DataLink's 200-with-error-text auth shapes
// (same family fetchDataLink guards against on main.cgi).
func isDataLinkAuthErrorBody(body string) bool {
	lower := strings.ToLower(body)
	if strings.HasPrefix(lower, "<") {
		return false // XML answers are classified structurally
	}
	return strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "access denied") ||
		strings.Contains(lower, "invalid username")
}

// parseSubscriptionManagementBody interprets one SMS response body:
//   - XML (any root, incl. <results>…</results> wrappers): every leaf element
//     is collected verbatim into fields; a document whose ONLY content is a
//     single bare code (e.g. <results>-3</results>) also surfaces it as
//     fields["results"].
//   - a bare token (returnXML ignored / legacy plain answers): returned as
//     bare, fields nil.
//
// Tolerant by design: unknown elements are preserved, never rejected; only
// genuinely undecodable bytes error.
func parseSubscriptionManagementBody(body string) (fields map[string]string, bare string, err error) {
	if !strings.HasPrefix(body, "<") {
		token := strings.TrimSpace(strings.Trim(body, `"`))
		if token == "" || strings.ContainsAny(token, "\n\r") {
			return nil, "", fmt.Errorf("unrecognized response shape")
		}
		return nil, token, nil
	}
	fields, err = parseSubscriptionManagementXML(body)
	if err != nil {
		return nil, "", err
	}
	return fields, "", nil
}

// parseSubscriptionManagementXML collects every leaf element (name -> trimmed
// char data) from an SMS XML document, ignoring nesting and unknown elements.
// Character data directly inside the ROOT element (e.g. <results>1</results>)
// is recorded under the root's own name.
func parseSubscriptionManagementXML(body string) (map[string]string, error) {
	dec := xml.NewDecoder(strings.NewReader(body))
	fields := map[string]string{}
	var stack []string
	depthText := map[int]*strings.Builder{}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			depthText[len(stack)] = &strings.Builder{}
		case xml.CharData:
			if len(stack) > 0 {
				depthText[len(stack)].Write(t)
			}
		case xml.EndElement:
			b := depthText[len(stack)]
			delete(depthText, len(stack))
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if b != nil {
				if v := strings.TrimSpace(b.String()); v != "" {
					fields[name] = v
				}
			}
		}
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("xml document carried no element values")
	}
	return fields, nil
}

// truncateForError bounds a provider body embedded in an error message.
func truncateForError(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
