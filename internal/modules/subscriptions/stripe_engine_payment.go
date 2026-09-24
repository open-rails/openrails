package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// StripeEnginePaymentParams is the immutable accepted operation, not browser input.
// Initial requires customer-present recurring consent. Later payments require the
// card's qualified recurring consent as its anchor: the initial PaymentIntent,
// or the off-session SetupIntent that saved a replacement card.
type StripeEnginePaymentParams struct {
	CustomerInitiated bool // explicit customer retry of an existing recurring agreement
	// OneTime is a customer-present purchase on a saved card: on-session, no
	// recurring agreement anchor and no future-usage setup.
	OneTime bool

	MerchantID, PSPID, CustomerID, OperationID uuid.UUID
	Instrument                                 charge.FrozenInstrument
	AmountMinor                                moneyutil.Cents
	Currency                                   string
	Initial                                    bool
}

type StripeEnginePaymentState string

const (
	StripeEnginePending                StripeEnginePaymentState = "pending"
	StripeEngineAuthenticationRequired StripeEnginePaymentState = "authentication_required"
	StripeEngineDeclined               StripeEnginePaymentState = "declined"
	StripeEngineSucceeded              StripeEnginePaymentState = "succeeded"
)

// No client secret is persisted in results, receipts, logs or ledger evidence.
type StripeEnginePaymentResult struct {
	DeclineCode     string                   `json:"decline_code,omitempty"`
	State           StripeEnginePaymentState `json:"state"`
	PaymentIntentID string                   `json:"payment_intent_id"`
	FailureCode     string                   `json:"failure_code,omitempty"`
	Receipt         *StripeEngineReceipt     `json:"receipt,omitempty"`
}

type StripeEngineReceipt struct {
	CustomerInitiated   bool            `json:"customer_initiated,omitempty"`
	OneTime             bool            `json:"one_time,omitempty"`
	RefundedAmountMinor moneyutil.Cents `json:"refunded_amount_minor,string,omitempty"`
	Refunded            bool            `json:"refunded,omitempty"`
	Disputed            bool            `json:"disputed,omitempty"`
	PaymentIntentID     string          `json:"payment_intent_id"`
	ChargeID            string          `json:"charge_id"`
	CustomerRef         string          `json:"customer_ref"`
	MethodRef           string          `json:"method_ref"`
	AmountMinor         moneyutil.Cents `json:"amount_minor,string"`
	Currency            string          `json:"currency"`
	MerchantID          uuid.UUID       `json:"merchant_id"`
	PSPID               uuid.UUID       `json:"psp_id"`
	CustomerID          uuid.UUID       `json:"customer_id"`
	OperationID         uuid.UUID       `json:"operation_id"`
	Initial             bool            `json:"initial"`
}

func (p StripeEnginePaymentParams) validate() error {
	if p.Initial && p.CustomerInitiated {
		return errors.New("initial enrollment cannot be a customer retry")
	}
	if p.OneTime && (p.Initial || p.CustomerInitiated) {
		return errors.New("one-time purchase cannot be a recurring payment")
	}
	if p.MerchantID == uuid.Nil || p.PSPID == uuid.Nil || p.CustomerID == uuid.Nil || p.OperationID == uuid.Nil || p.Instrument.PSPID != p.PSPID || p.Instrument.Custodian != models.CustodianPSP || p.Instrument.CustodianID != nil || !stripeEngineID(p.Instrument.RailCustomerRef, "cus_") || !stripeEngineID(p.Instrument.RailMethodRef, "pm_") || p.AmountMinor <= 0 || p.AmountMinor > 99999999 {
		return errors.New("incomplete Stripe engine operation binding")
	}
	if err := moneyutil.ValidateCurrency(p.Currency); err != nil {
		return err
	}
	if !p.Initial && !p.OneTime && !stripeEngineID(p.Instrument.StoredCredentialRecurringRef, "pi_") && !stripeEngineID(p.Instrument.StoredCredentialRecurringRef, "seti_") {
		return errors.New("Stripe engine payment lacks a qualified recurring agreement")
	}
	return nil
}
func stripeEngineID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 255 {
		return false
	}
	for _, c := range id[len(prefix):] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func (s *StripeService) engineScoped(p StripeEnginePaymentParams) (*StripeService, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if s == nil || s.Config == nil || s.accountMerchantID != p.MerchantID || s.accountPSPID != p.PSPID || s.accountID == "" || s.accountSecret == "" {
		return nil, errors.New("Stripe engine account does not match accepted operation")
	}
	scoped := *s
	scoped.Rails = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: s.accountID, Stripe: &config.StripeRailConfig{SecretKey: s.accountSecret}}}
	return &scoped, nil
}
func (p StripeEnginePaymentParams) metadata() map[string]string {
	values := map[string]string{"openrails_engine_operation": p.OperationID.String(), "openrails_merchant": p.MerchantID.String(), "openrails_psp": p.PSPID.String(), "openrails_customer": p.CustomerID.String(), "openrails_initial": strconv.FormatBool(p.Initial), "openrails_agreement": p.Instrument.StoredCredentialRecurringRef}
	if p.CustomerInitiated {
		values["openrails_customer_retry"] = "true"
	}
	if p.OneTime {
		values["openrails_one_time"] = "true"
	}
	return values
}

// CreateEnginePayment is called only by the winner of a durable submission or
// resend fence. A resend follows a settled, empty ReadEnginePayment and reuses
// the operation's idempotency key, so Stripe replays any original it holds.
func (s *StripeService) CreateEnginePayment(ctx context.Context, p StripeEnginePaymentParams) (StripeEnginePaymentResult, error) {
	scoped, err := s.engineScoped(p)
	if err != nil {
		return StripeEnginePaymentResult{}, fmt.Errorf("%w: %v", charge.ErrNotDispatched, err)
	}
	v := url.Values{"amount": {strconv.FormatInt(int64(p.AmountMinor), 10)}, "currency": {strings.ToLower(p.Currency)}, "customer": {p.Instrument.RailCustomerRef}, "payment_method": {p.Instrument.RailMethodRef}, "payment_method_types[]": {"card"}, "confirm": {"true"}, "capture_method": {"automatic"}, "confirmation_method": {"automatic"}, "off_session": {strconv.FormatBool(!p.Initial && !p.CustomerInitiated && !p.OneTime)}}
	if p.Initial {
		v.Set("setup_future_usage", "off_session")
	}
	for key, value := range p.metadata() {
		v.Set("metadata["+key+"]", value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scoped.stripeBaseURL()+"/v1/payment_intents", strings.NewReader(v.Encode()))
	if err != nil {
		return StripeEnginePaymentResult{}, fmt.Errorf("%w: %v", charge.ErrNotDispatched, err)
	}
	req.Header.Set("Authorization", "Bearer "+scoped.accountSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stripeapi.SetIdempotencyKey(req, "engine:"+p.OperationID.String())
	resp, err := scoped.StripeClients.Client(scoped.Config, 0).Do(req)
	if errors.Is(err, stripeapi.ErrProviderReadOnly) {
		return StripeEnginePaymentResult{}, fmt.Errorf("%w: %v", charge.ErrNotDispatched, err)
	}
	if err != nil {
		return StripeEnginePaymentResult{}, errors.New("Stripe engine submission outcome unknown")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return StripeEnginePaymentResult{}, errors.New("Stripe engine response unreadable")
	}
	if resp.StatusCode >= 400 {
		// Card errors can contain the very same PI that needs authentication.
		// Never log/return the raw provider body (it may contain a client secret).
		var envelope struct {
			Error struct {
				PaymentIntent json.RawMessage `json:"payment_intent"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) != nil || len(envelope.Error.PaymentIntent) == 0 || resp.StatusCode != http.StatusPaymentRequired {
			return StripeEnginePaymentResult{}, fmt.Errorf("Stripe engine submission unresolved (HTTP %d)", resp.StatusCode)
		}
		body = envelope.Error.PaymentIntent
	}
	var pi stripeEngineIntent
	if json.Unmarshal(body, &pi) != nil {
		return StripeEnginePaymentResult{}, errors.New("Stripe engine response malformed")
	}
	return scoped.engineResult(ctx, p, pi)
}

type stripeEngineIntent struct {
	Object             string            `json:"object"`
	LiveMode           *bool             `json:"livemode"`
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	Customer           json.RawMessage   `json:"customer"`
	PaymentMethod      json.RawMessage   `json:"payment_method"`
	Amount             int64             `json:"amount"`
	AmountReceived     int64             `json:"amount_received"`
	Currency           string            `json:"currency"`
	SetupFutureUsage   string            `json:"setup_future_usage"`
	CaptureMethod      string            `json:"capture_method"`
	ConfirmationMethod string            `json:"confirmation_method"`
	LatestCharge       json.RawMessage   `json:"latest_charge"`
	Metadata           map[string]string `json:"metadata"`
	ClientSecret       string            `json:"client_secret"`
	CancellationReason string            `json:"cancellation_reason"`
	LastPaymentError   *struct {
		Code          string          `json:"code"`
		DeclineCode   string          `json:"decline_code"`
		PaymentMethod json.RawMessage `json:"payment_method"`
	} `json:"last_payment_error"`
}

func (pi stripeEngineIntent) matches(p StripeEnginePaymentParams) error {
	method := rawID(pi.PaymentMethod)
	// Stripe clears payment_method on some failed off-session attempts.
	if method == "" && pi.LastPaymentError != nil {
		method = rawID(pi.LastPaymentError.PaymentMethod)
	}
	// A canceled or unpaid PI can drop its method (Stripe clears it with the
	// last error on cancel); it moves no money, and a paid PI must name it.
	methodMismatch := method != p.Instrument.RailMethodRef && (method != "" || pi.Status == "succeeded")
	if !stripeEngineID(pi.ID, "pi_") || rawID(pi.Customer) != p.Instrument.RailCustomerRef || methodMismatch || pi.Amount != int64(p.AmountMinor) || !strings.EqualFold(pi.Currency, p.Currency) || pi.CaptureMethod != "automatic" || pi.ConfirmationMethod != "automatic" || p.Initial && pi.SetupFutureUsage != "off_session" {
		return errors.New("Stripe engine payment does not match frozen terms")
	}
	for k, v := range p.metadata() {
		if pi.Metadata[k] != v {
			return errors.New("Stripe engine payment does not match accepted operation")
		}
	}
	return nil
}

// ValidateStripeEnginePaymentNotification uses the receipt parser's exact
// accepted-term binding for a wake-up signal. It grants no payment authority;
// the operation worker still fetches and qualifies the current provider state.
func ValidateStripeEnginePaymentNotification(raw []byte, params StripeEnginePaymentParams, environment string) error {
	var pi stripeEngineIntent
	if err := json.Unmarshal(raw, &pi); err != nil {
		return err
	}
	if pi.Object != "payment_intent" {
		return errors.New("Stripe notification resource is not a PaymentIntent")
	}
	if environment != "live" && environment != "test" {
		return errors.New("Stripe notification account environment is unknown")
	}
	if pi.LiveMode == nil || *pi.LiveMode != (environment == "live") {
		return errors.New("Stripe notification environment differs from routed account")
	}
	return pi.matches(params)
}

// ReadEnginePayment never writes. A missing candidate triggers a fully paginated
// customer list, not Stripe's eventually consistent Search API. Absence is
// Stripe's answer only after the caller's settle delay.
func (s *StripeService) ReadEnginePayment(ctx context.Context, p StripeEnginePaymentParams, reference string) (StripeEnginePaymentResult, bool, error) {
	scoped, err := s.engineScoped(p)
	if err != nil {
		return StripeEnginePaymentResult{}, false, err
	}
	pi, found, err := scoped.readEngineIntent(ctx, p, reference)
	if err != nil || !found {
		return StripeEnginePaymentResult{}, found, err
	}
	result, err := scoped.engineResult(ctx, p, pi)
	return result, true, err
}
func (s *StripeService) readEngineIntent(ctx context.Context, p StripeEnginePaymentParams, reference string) (stripeEngineIntent, bool, error) {
	var pi stripeEngineIntent
	if reference == "" {
		count := 0
		err := s.stripeListAll(ctx, "/v1/payment_intents", url.Values{"customer": {p.Instrument.RailCustomerRef}}, func(raw json.RawMessage) error {
			var candidate stripeEngineIntent
			if err := json.Unmarshal(raw, &candidate); err != nil {
				return errors.New("Stripe engine list malformed")
			}
			if candidate.Metadata["openrails_engine_operation"] == p.OperationID.String() {
				count++
				pi = candidate
			}
			return nil
		})
		if err != nil {
			return pi, false, err
		}
		if count > 1 {
			return pi, false, errors.New("multiple Stripe payments claim accepted operation")
		}
		if count == 0 {
			return pi, false, nil
		}
		reference = pi.ID
	}
	if !stripeEngineID(reference, "pi_") {
		return pi, false, errors.New("invalid Stripe engine payment reference")
	}
	body, status, err := s.stripeGet(ctx, "/v1/payment_intents/"+url.PathEscape(reference), nil)
	if err != nil {
		return pi, false, errors.New("Stripe engine read failed")
	}
	if status == http.StatusNotFound {
		return pi, false, nil
	}
	if status >= 400 {
		return pi, false, fmt.Errorf("Stripe engine read failed (HTTP %d)", status)
	}
	if json.Unmarshal(body, &pi) != nil {
		return pi, false, errors.New("Stripe engine read malformed")
	}
	if pi.ID != reference {
		return pi, true, errors.New("Stripe engine payment identity mismatch")
	}
	return pi, true, pi.matches(p)
}
func (s *StripeService) engineResult(ctx context.Context, p StripeEnginePaymentParams, pi stripeEngineIntent) (StripeEnginePaymentResult, error) {
	r := StripeEnginePaymentResult{State: StripeEnginePending, PaymentIntentID: pi.ID}
	if pi.LiveMode == nil || *pi.LiveMode == s.Config.IsTestMode() {
		return StripeEnginePaymentResult{}, errors.New("Stripe engine payment environment mismatch")
	}
	if err := pi.matches(p); err != nil {
		return StripeEnginePaymentResult{}, err
	}
	switch pi.Status {
	case "requires_action":
		r.State = StripeEngineAuthenticationRequired
	case "requires_payment_method":
		if pi.LastPaymentError != nil {
			r.FailureCode = pi.LastPaymentError.DeclineCode
			if r.FailureCode == "" {
				r.FailureCode = pi.LastPaymentError.Code
			}
			if pi.LastPaymentError.Code == "authentication_required" || pi.LastPaymentError.DeclineCode == "authentication_required" {
				r.State = StripeEngineAuthenticationRequired
			} else if r.FailureCode != "" {
				r.State = StripeEngineDeclined
			}
		}
	case "canceled":
		r.State = StripeEngineDeclined
		r.FailureCode = "canceled"
		if pi.LastPaymentError != nil {
			r.DeclineCode = pi.LastPaymentError.DeclineCode
			if r.DeclineCode == "" {
				r.DeclineCode = pi.LastPaymentError.Code
			}
		}
		if r.DeclineCode == "" && pi.CancellationReason == "abandoned" {
			r.DeclineCode = "authentication_required"
		}
	case "succeeded":
		if pi.AmountReceived != int64(p.AmountMinor) {
			return r, errors.New("Stripe received amount differs from accepted amount")
		}
		receipt, err := s.engineReceipt(ctx, p, pi)
		if err != nil {
			return r, err
		}
		r.State = StripeEngineSucceeded
		r.Receipt = &receipt
	}
	return r, nil
}
func (s *StripeService) engineReceipt(ctx context.Context, p StripeEnginePaymentParams, pi stripeEngineIntent) (StripeEngineReceipt, error) {
	ref := rawID(pi.LatestCharge)
	if !stripeEngineID(ref, "ch_") {
		return StripeEngineReceipt{}, errors.New("Stripe engine payment has no captured charge")
	}
	body, status, err := s.stripeGet(ctx, "/v1/charges/"+url.PathEscape(ref), nil)
	if err != nil || status >= 400 {
		return StripeEngineReceipt{}, errors.New("Stripe engine captured charge unreadable")
	}
	var ch struct {
		ID             string          `json:"id"`
		Amount         int64           `json:"amount"`
		AmountCaptured int64           `json:"amount_captured"`
		Currency       string          `json:"currency"`
		Customer       json.RawMessage `json:"customer"`
		PaymentMethod  string          `json:"payment_method"`
		PaymentIntent  json.RawMessage `json:"payment_intent"`
		Status         string          `json:"status"`
		Paid           bool            `json:"paid"`
		Captured       bool            `json:"captured"`
		Refunded       bool            `json:"refunded"`
		Disputed       bool            `json:"disputed"`
		AmountRefunded int64           `json:"amount_refunded"`
	}
	if json.Unmarshal(body, &ch) != nil || ch.ID != ref || ch.Amount != int64(p.AmountMinor) || ch.AmountCaptured != int64(p.AmountMinor) || !strings.EqualFold(ch.Currency, p.Currency) || rawID(ch.Customer) != p.Instrument.RailCustomerRef || ch.PaymentMethod != p.Instrument.RailMethodRef || rawID(ch.PaymentIntent) != pi.ID || ch.Status != "succeeded" || !ch.Paid || !ch.Captured || ch.AmountRefunded < 0 || ch.AmountRefunded > ch.Amount || (ch.Refunded && ch.AmountRefunded != ch.Amount) {
		return StripeEngineReceipt{}, errors.New("Stripe engine captured charge does not match accepted payment")
	}
	return StripeEngineReceipt{PaymentIntentID: pi.ID, ChargeID: ref, CustomerRef: p.Instrument.RailCustomerRef, MethodRef: p.Instrument.RailMethodRef, AmountMinor: p.AmountMinor, Currency: p.Currency, MerchantID: p.MerchantID, PSPID: p.PSPID, CustomerID: p.CustomerID, OperationID: p.OperationID, Initial: p.Initial, CustomerInitiated: p.CustomerInitiated, OneTime: p.OneTime, RefundedAmountMinor: moneyutil.Cents(ch.AmountRefunded), Refunded: ch.Refunded, Disputed: ch.Disputed}, nil
}
func (r StripeEngineReceipt) Matches(p StripeEnginePaymentParams) error {
	if err := p.validate(); err != nil {
		return err
	}
	if r.RefundedAmountMinor < 0 || r.RefundedAmountMinor > r.AmountMinor || (r.Refunded && r.RefundedAmountMinor != r.AmountMinor) {
		return errors.New("Stripe charge reversal facts are inconsistent")
	}
	if !stripeEngineID(r.PaymentIntentID, "pi_") || !stripeEngineID(r.ChargeID, "ch_") || r.CustomerRef != p.Instrument.RailCustomerRef || r.MethodRef != p.Instrument.RailMethodRef || r.AmountMinor != p.AmountMinor || r.Currency != p.Currency || r.MerchantID != p.MerchantID || r.PSPID != p.PSPID || r.CustomerID != p.CustomerID || r.OperationID != p.OperationID || r.Initial != p.Initial || r.CustomerInitiated != p.CustomerInitiated || r.OneTime != p.OneTime {
		return errors.New("Stripe engine receipt differs from accepted operation")
	}
	return nil
}

// EngineAuthenticationSecret returns only the original PI secret after exact
// account/terms binding and explicit customer identity authorization. Callers
// must require a live authenticated customer session, set Cache-Control:no-store,
// and pass this secret only to Stripe.js to authenticate the frozen card. Card
// replacement requires a separately authorized new agreement; it cannot mutate
// this accepted operation. This function performs no provider writes.
func (s *StripeService) EngineAuthenticationSecret(ctx context.Context, p StripeEnginePaymentParams, reference string, customerID uuid.UUID) (string, error) {
	if customerID == uuid.Nil || customerID != p.CustomerID {
		return "", errors.New("Stripe recovery customer mismatch")
	}
	scoped, err := s.engineScoped(p)
	if err != nil {
		return "", err
	}
	if reference == "" {
		return "", errors.New("Stripe recovery requires retained payment identity")
	}
	pi, found, err := scoped.readEngineIntent(ctx, p, reference)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("Stripe recovery payment missing")
	}
	r, err := scoped.engineResult(ctx, p, pi)
	if err != nil {
		return "", err
	}
	if r.State != StripeEngineAuthenticationRequired || !strings.HasPrefix(pi.ClientSecret, pi.ID+"_secret_") {
		return "", errors.New("Stripe payment is not awaiting authentication")
	}
	return pi.ClientSecret, nil
}

// FinalizeEngineDecline closes the SAME failed PI before permitting a new
// obligation/attempt. A previously issued client secret must not remain capable
// of paying after local terminal refusal. Authentication-required and uncertain
// states are never canceled automatically. A lost cancel response is recovered
// by reading this PI; no financial create or confirm is performed here.
func (s *StripeService) FinalizeEngineDecline(ctx context.Context, p StripeEnginePaymentParams, reference string) (StripeEnginePaymentResult, error) {
	scoped, err := s.engineScoped(p)
	if err != nil {
		return StripeEnginePaymentResult{}, err
	}
	result, found, err := scoped.ReadEnginePayment(ctx, p, reference)
	if err != nil {
		return result, err
	}
	if !found || result.State != StripeEngineDeclined {
		return result, errors.New("Stripe engine payment has no definitive decline")
	}
	if result.FailureCode == "canceled" {
		return result, nil
	}
	if _, err := scoped.stripePostForm(ctx, "/v1/payment_intents/"+url.PathEscape(result.PaymentIntentID)+"/cancel", url.Values{}, "engine:"+p.OperationID.String()+":cancel"); err != nil {
		return StripeEnginePaymentResult{}, errors.New("Stripe engine cancellation requires same-payment readback")
	}
	canceled, found, err := scoped.ReadEnginePayment(ctx, p, result.PaymentIntentID)
	if err != nil {
		return canceled, err
	}
	if !found || canceled.State != StripeEngineDeclined || canceled.FailureCode != "canceled" {
		return canceled, errors.New("Stripe engine decline remains executable")
	}
	if canceled.DeclineCode == "" {
		canceled.DeclineCode = result.FailureCode
	}
	return canceled, nil
}

// CancelAbandonedEnginePayment closes the SAME payment after its issuer
// authentication window lapsed, so a challenge the payer never finished can
// no longer charge them and the accepted operation can resolve. It reads
// first and cancels only a payment still awaiting authentication; a payer
// who completed meanwhile reads back succeeded.
func (s *StripeService) CancelAbandonedEnginePayment(ctx context.Context, p StripeEnginePaymentParams, reference string) (StripeEnginePaymentResult, error) {
	scoped, err := s.engineScoped(p)
	if err != nil {
		return StripeEnginePaymentResult{}, err
	}
	result, found, err := scoped.ReadEnginePayment(ctx, p, reference)
	if err != nil {
		return result, err
	}
	if !found || result.State != StripeEngineAuthenticationRequired {
		return result, nil
	}
	if _, err := scoped.stripePostForm(ctx, "/v1/payment_intents/"+url.PathEscape(result.PaymentIntentID)+"/cancel", url.Values{"cancellation_reason": {"abandoned"}}, "engine:"+p.OperationID.String()+":abandon"); err != nil {
		return StripeEnginePaymentResult{}, errors.New("Stripe abandoned-authentication cancel requires same-payment readback")
	}
	closed, _, err := scoped.ReadEnginePayment(ctx, p, result.PaymentIntentID)
	return closed, err
}

// ReversalKind preserves the original capture while withholding fresh access.
// Existing refund/dispute convergence records the separate reversal after the
// original charge is present locally; it must never be treated as a new charge.
func (r StripeEngineReceipt) ReversalKind() string {
	if r.Disputed {
		return "dispute"
	}
	if r.Refunded || (r.AmountMinor > 0 && r.RefundedAmountMinor == r.AmountMinor) {
		return "refund"
	}
	return ""
}
