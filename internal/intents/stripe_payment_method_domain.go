package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
)

// TypeStripePaymentMethodDomain registers the hosted checkout's host as a
// payment-method domain on a Stripe PSP's account, so Stripe's Express
// Checkout Element offers Apple Pay and Google Pay there.
const TypeStripePaymentMethodDomain = "stripe_payment_method_domain"

// StripePaymentMethodDomainPayload is the stored payload.
type StripePaymentMethodDomainPayload struct {
	DomainName string `json:"domain_name"`
}

// StripePaymentMethodDomainKey: one registration per PSP and host.
func StripePaymentMethodDomainKey(psp uuid.UUID, domain string) string {
	return TypeStripePaymentMethodDomain + ":" + psp.String() + ":" + domain
}

const stripeDomainDenialMaxAttempts = 3

// StripePaymentMethodDomainHandler reads first: a domain Stripe already holds
// is not registered again, and one whose Apple Pay is inactive is validated
// once. A domain the merchant disabled at Stripe stays disabled.
type StripePaymentMethodDomainHandler struct {
	Config  *config.Config
	Rails   railresolve.Source
	Clients *stripeapi.Factory
	Policy  BackoffPolicy
}

func NewStripePaymentMethodDomainHandler(cfg *config.Config, rails railresolve.Source, clients *stripeapi.Factory) *StripePaymentMethodDomainHandler {
	return &StripePaymentMethodDomainHandler{Config: cfg, Rails: rails, Clients: clients, Policy: DefaultBackoff}
}

func (h *StripePaymentMethodDomainHandler) Type() string { return TypeStripePaymentMethodDomain }
func (h *StripePaymentMethodDomainHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func (h *StripePaymentMethodDomainHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (Relevance, error) {
	return StillRelevant(), nil
}

// stripeDomain is the part of Stripe's payment_method_domain object read.
type stripeDomain struct {
	ID       string `json:"id"`
	Domain   string `json:"domain_name"`
	Enabled  bool   `json:"enabled"`
	ApplePay struct {
		Status        string `json:"status"`
		StatusDetails struct {
			ErrorMessage string `json:"error_message"`
		} `json:"status_details"`
	} `json:"apple_pay"`
	GooglePay struct {
		Status string `json:"status"`
	} `json:"google_pay"`
}

func (d stripeDomain) evidence() map[string]any {
	ev := map[string]any{"payment_method_domain": d.ID, "domain_name": d.Domain, "enabled": d.Enabled,
		"apple_pay": d.ApplePay.Status, "google_pay": d.GooglePay.Status}
	if msg := d.ApplePay.StatusDetails.ErrorMessage; msg != "" {
		ev["apple_pay_error"] = msg
	}
	return ev
}

func (h *StripePaymentMethodDomainHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	domain, key, err := h.target(ctx, intent)
	if err != nil {
		return Parked("stripe not configured: " + err.Error())
	}
	found, err := h.find(ctx, key, domain)
	if err != nil {
		return Retryable("stripe read before registering failed: " + err.Error())
	}
	if found != nil {
		return h.held(ctx, intent, key, *found)
	}
	status, raw, err := h.post(ctx, key, "/v1/payment_method_domains", url.Values{"domain_name": {domain}}, intent.IdempotencyKey)
	if outcome, done := h.classify(intent, status, raw, err); done {
		return outcome
	}
	if status < 300 {
		var created stripeDomain
		if err := json.Unmarshal(raw, &created); err != nil {
			return Ambiguous("stripe registered the domain; answer unreadable: " + err.Error())
		}
		return Succeeded(created.evidence())
	}
	// A refusal after an empty read: the domain is Stripe's to judge, unless
	// another writer registered it meanwhile.
	if found, err := h.find(ctx, key, domain); err == nil && found != nil {
		return Succeeded(found.evidence())
	}
	return Terminal(fmt.Sprintf("stripe refused the domain %s (%d): %s", domain, status, subscriptions.ParseStripeAPIError(raw)))
}

// Verify reads: a registered domain is success; none means the create did
// not apply and may be sent again.
func (h *StripePaymentMethodDomainHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	domain, key, err := h.target(ctx, intent)
	if err != nil {
		return Ambiguous("stripe not configured; cannot verify: " + err.Error())
	}
	found, err := h.find(ctx, key, domain)
	if err != nil {
		return Ambiguous("stripe read failed: " + err.Error())
	}
	if found == nil {
		return Retryable("domain not registered at Stripe; registration verified not applied")
	}
	ev := found.evidence()
	ev["via_verifier"] = true
	return Succeeded(ev)
}

// held settles a domain Stripe already holds, validating an enabled one whose
// Apple Pay is not active.
func (h *StripePaymentMethodDomainHandler) held(ctx context.Context, intent gen.BillingProviderIntent, key string, d stripeDomain) Outcome {
	if !d.Enabled || d.ApplePay.Status == "active" || d.ID == "" {
		return Succeeded(d.evidence())
	}
	status, raw, err := h.post(ctx, key, "/v1/payment_method_domains/"+url.PathEscape(d.ID)+"/validate", nil, intent.IdempotencyKey+":validate")
	if outcome, done := h.classify(intent, status, raw, err); done {
		return outcome
	}
	ev := d.evidence()
	if status < 300 {
		var validated stripeDomain
		if json.Unmarshal(raw, &validated) == nil && validated.ID != "" {
			ev = validated.evidence()
		}
	} else {
		ev["validate_error"] = subscriptions.ParseStripeAPIError(raw)
	}
	return Succeeded(ev)
}

// classify answers the outcomes every write shares; done=false leaves a
// 2xx or another 4xx to the caller.
func (h *StripePaymentMethodDomainHandler) classify(intent gen.BillingProviderIntent, status int, raw []byte, err error) (Outcome, bool) {
	msg := subscriptions.ParseStripeAPIError(raw)
	switch {
	case errors.Is(err, stripeapi.ErrProviderReadOnly):
		return Parked("stripe provider writes blocked (mode=readonly)"), true
	case errors.Is(err, providerposture.ErrDisarmed):
		return Parked("stripe domain registration held by sandbox posture gate: " + err.Error()), true
	case err != nil:
		return Ambiguous("stripe domain registration outcome unknown: " + err.Error()), true
	case status == http.StatusTooManyRequests:
		return Retryable("stripe rate limited: " + msg), true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if intent.Attempts >= stripeDomainDenialMaxAttempts {
			return Terminal(fmt.Sprintf("stripe refused the domain registration after bounded retries (%d): %s", status, msg)), true
		}
		return Retryable(fmt.Sprintf("stripe refused the domain registration (%d): %s", status, msg)), true
	case status >= 500:
		return Ambiguous(fmt.Sprintf("stripe domain registration answered %d: %s", status, msg)), true
	}
	return Outcome{}, false
}

// target is the intent's domain and the secret key of the PSP it names.
func (h *StripePaymentMethodDomainHandler) target(ctx context.Context, intent gen.BillingProviderIntent) (string, string, error) {
	var p StripePaymentMethodDomainPayload
	if err := json.Unmarshal(intent.Payload, &p); err != nil || strings.TrimSpace(p.DomainName) == "" {
		return "", "", fmt.Errorf("payload names no domain")
	}
	if intent.PspID == nil {
		return "", "", fmt.Errorf("intent names no PSP")
	}
	_, key, err := subscriptions.RequireStripeSecretKey(db.WithPSPID(ctx, *intent.PspID), h.Rails)
	return p.DomainName, key, err
}

func (h *StripePaymentMethodDomainHandler) find(ctx context.Context, key, domain string) (*stripeDomain, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeapi.APIBase+"/v1/payment_method_domains?"+url.Values{"domain_name": {domain}, "limit": {"1"}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := h.Clients.ReadOnlyClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stripe answered %d: %s", resp.StatusCode, subscriptions.ParseStripeAPIError(raw))
	}
	var page struct {
		Data []stripeDomain `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	for _, d := range page.Data {
		if strings.EqualFold(d.Domain, domain) {
			return &d, nil
		}
	}
	return nil, nil
}

func (h *StripePaymentMethodDomainHandler) post(ctx context.Context, key, path string, form url.Values, idempotencyKey string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stripeapi.APIBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stripeapi.SetIdempotencyKey(req, idempotencyKey)
	resp, err := h.Clients.Client(h.Config, 0).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
