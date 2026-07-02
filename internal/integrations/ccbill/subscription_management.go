package ccbill

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// #696: DataLink Subscription Management System — per-subscription read
// (viewSubscriptionStatus) + merchant-initiated cancel (cancelSubscription)
// via POST {BaseURL}/utils/subscriptionManagement.cgi.
//
// WIRE SHAPE PROVISIONAL (Phase 0 live probe pending): params and response
// fields follow the long-lived third-party DataLink integrations (s2member,
// YourMembers, Magic Members) — CCBill's own spec is portal-gated. Request
// building (subscriptionManagementForm) and response parsing
// (parseSubscriptionManagementFields / classification below) are deliberately
// isolated so post-probe adjustments stay in this one file.

const subscriptionManagementPath = "/utils/subscriptionManagement.cgi"

// SMS actions.
const (
	actionViewSubscriptionStatus = "viewSubscriptionStatus"
	actionCancelSubscription     = "cancelSubscription"
)

// ErrProviderReadOnly is returned by every CCBill mutation when the provider
// is read-only (mode=readonly, #346) — blocked before any bytes hit the wire,
// mirroring nmi.ErrProviderReadOnly.
var ErrProviderReadOnly = errors.New("ccbill: provider writes are blocked (mode=readonly)")

// ErrDataLinkAuth marks a definite authentication/authorization rejection
// (HTTP 401/403 or an auth-shaped error body). The request was NOT executed.
var ErrDataLinkAuth = errors.New("ccbill datalink: authentication rejected")

// ErrCancelRejected marks a received, parsed, DEFINITE non-success answer to
// cancelSubscription (results != 1). The verbatim provider answer is wrapped.
// Callers on the intents path verify-not-decline: a reject may mean
// already-cancelled — the viewSubscriptionStatus re-read decides.
var ErrCancelRejected = errors.New("ccbill datalink: cancelSubscription rejected")

// maxSubscriptionManagementResponseBytes bounds one SMS response (tiny XML).
const maxSubscriptionManagementResponseBytes = 1 << 20

// subscriptionStatusExpiryFields are the response fields consulted (in order)
// for the paid-through / expiry date. PROVISIONAL until Phase 0.
var subscriptionStatusExpiryFields = []string{"expirationDate", "expireDate", "nextRenewalDate"}

// subscriptionStatusDateLayouts are the accepted expiry encodings.
var subscriptionStatusDateLayouts = []string{"20060102", "2006-01-02 15:04:05", "2006-01-02", "01/02/2006"}

// SubscriptionStatusResult is one parsed viewSubscriptionStatus answer.
// RawStatus is the verbatim <subscriptionStatus> value (required — a response
// without it is an error, never a defaulted struct); Fields preserves every
// response leaf verbatim.
type SubscriptionStatusResult struct {
	RawStatus string
	Fields    map[string]string
}

// Rebilling reports whether CCBill will attempt future rebills.
// PROVISIONAL status vocabulary (legacy DataLink SMS docs):
//
//	"2" = active, recurring        -> will rebill
//	"1" = active, non-recurring    -> no rebill (cancelled-with-runway / one-time)
//	"0" = inactive / expired       -> no rebill
//
// Unrecognized values are an ERROR (#651: never silently mapped).
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

// CancelResult carries the verbatim provider answer to cancelSubscription.
type CancelResult struct {
	Results string // verbatim results token ("1" on success)
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
		return SubscriptionStatusResult{}, fmt.Errorf("ccbill viewSubscriptionStatus: provider answered bare code %q instead of a status document", bare)
	}
	raw, ok := fields["subscriptionStatus"]
	if !ok || strings.TrimSpace(raw) == "" {
		if code, hasCode := fields["results"]; hasCode {
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
//	ErrDataLinkAuth     — definite auth rejection, not executed (clean retry)
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
	return CancelResult{Results: results}, fmt.Errorf("%w: results=%q", ErrCancelRejected, results)
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

// postSubscriptionManagement performs ONE SMS round-trip (no retry loop — the
// intents executor/verifier own retries; blind HTTP retries on a mutation
// endpoint are unsafe) and returns the raw trimmed body. Auth-shaped
// rejections (401/403, auth-error body) surface as ErrDataLinkAuth.
func (c *DataLinkClient) postSubscriptionManagement(ctx context.Context, action, subscriptionID string) (string, error) {
	form := c.subscriptionManagementForm(action, subscriptionID)
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
