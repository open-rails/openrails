package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Checkout sessions (#1124). A session is minted for one customer and one
// price; its id then reads and pays it with no other credential. Paying runs a
// checkout attempt, one idempotency key per attempt.

var errHostedOfferUnavailable = apperr.New(http.StatusUnprocessableEntity, "checkout_offer_unavailable", "This purchase is not available.")

// CheckoutSessionMint is a mint request. Advertise attaches the browser driver
// and public values to the price's options (the HTTP layer's checkout-config
// projection); an option the page cannot drive is not offered.
type CheckoutSessionMint struct {
	billing.CreateCheckoutSessionParams
	Advertise func([]billing.CheckoutOption)
}

// CreateCheckoutSession mints a session for the customer and price.
func (s *Service) CreateCheckoutSession(ctx context.Context, in CheckoutSessionMint) (*billing.CheckoutSessionLink, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.hostedRuntime()
	if err != nil {
		return nil, err
	}
	customerID := in.Customer.ID
	if customerID.IsZero() {
		return nil, apperr.Invalidf("valid customer id required").WithParam("customer.id")
	}
	if _, err := hostedBuyer(ctx, rt, customerID, checkoutsession.Buyer{}); err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(in.SuccessURL)
	if successURL != "" && !config.ReturnURLAllowed(rt.Config, successURL) {
		return nil, apperr.Invalidf("success_url origin is not allowed").WithParam("success_url")
	}
	offer, priceID, err := s.hostedOffer(ctx, rt, in)
	if err != nil {
		return nil, err
	}
	offer.Buyer = checkoutsession.Buyer{VerifiedEmail: strings.TrimSpace(in.Customer.VerifiedEmail), Username: strings.TrimSpace(in.Customer.Username)}
	id, err := checkoutsession.NewID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	session := checkoutsession.Session{
		CustomerID: customerID.UUID(), PriceID: priceID, Offer: offer, SuccessURL: successURL,
		Origin: hostedAppOrigin(rt.Config, successURL), ExpiresAt: now.Add(checkoutsession.SessionTTL),
	}
	if err := rt.CheckoutSessions.Create(ctx, id, session, now); err != nil {
		return nil, fmt.Errorf("create checkout session: %w", err)
	}
	link := &billing.CheckoutSessionLink{ID: id, ExpiresAt: session.ExpiresAt}
	if page := strings.TrimSpace(config.PublishedCheckout(rt.Config).PageURL); page != "" {
		pageURL := page + "#" + id
		link.URL = &pageURL
	}
	return link, nil
}

// GetCheckoutSession is the session document for its id.
func (s *Service) GetCheckoutSession(ctx context.Context, id string) (*checkoutsession.CheckoutSession, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.hostedRuntime()
	if err != nil {
		return nil, err
	}
	now := s.now()
	session, err := rt.CheckoutSessions.Get(ctx, strings.TrimSpace(id), now)
	if err != nil {
		return nil, err
	}
	if _, err := hostedBuyer(ctx, rt, billing.CustomerID(session.CustomerID), session.Offer.Buyer); err != nil {
		return nil, err
	}
	state := &billing.CheckoutAttempt{Status: billing.CheckoutAttemptCreated}
	if session.AttemptID != nil {
		if state, err = s.getCheckoutAttempt(ctx, billing.CheckoutAttemptID(*session.AttemptID)); err != nil {
			return nil, fmt.Errorf("read checkout session payment: %w", err)
		}
	}
	status := hostedStatus(string(state.Status))
	if hostedOver(session, status, now) {
		status = "expired"
	}
	due, tax := session.Offer.DueToday, int64(0)
	out := &checkoutsession.CheckoutSession{
		ID: session.ID, Status: status,
		Merchant:  checkoutsession.CheckoutSessionMerchant{DisplayName: session.Offer.MerchantDisplayName},
		Plan:      session.Offer.Plan,
		LineItems: []checkoutsession.CheckoutSessionLineItem{{Label: session.Offer.Plan.DisplayName, Amount: due}},
		Tax:       &tax, DueToday: &due,
		Options:        make([]checkoutsession.CheckoutSessionOption, 0, len(session.Offer.Options)),
		NextAction:     hostedNextAction(state),
		Operation:      hostedOperation(state),
		PaymentID:      state.PaymentID,
		SubscriptionID: state.SubscriptionID,
		FailureMessage: hostedFailureMessage(status),
		Failure:        state.Failure,
		ExpiresAt:      session.ExpiresAt,
	}
	if session.SuccessURL != "" {
		out.SuccessURL = &session.SuccessURL
	}
	for _, option := range session.Offer.Options {
		out.Options = append(out.Options, option.CheckoutSessionOption)
	}
	if config.CheckoutEmbedAllowed(rt.Config, session.Origin) {
		out.EmbedOrigin = &session.Origin
	}
	if out.SavedMethods, err = hostedSavedMethods(ctx, rt, session); err != nil {
		return nil, err
	}
	return out, nil
}

// PayCheckoutSession starts or resumes the session's current attempt.
// Every submission of one attempt sends the engine the same idempotency key,
// so repeated and concurrent submissions charge at most once. The attempt
// advances only after the engine reports it terminally failed; an ambiguous
// error keeps it, and the next submission replays the same key.
func (s *Service) PayCheckoutSession(ctx context.Context, id string, input checkoutsession.PayCheckoutSessionParams, clientIP string) (*checkoutsession.CheckoutSessionPayResult, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.hostedRuntime()
	if err != nil {
		return nil, err
	}
	now := s.now()
	session, err := rt.CheckoutSessions.Get(ctx, strings.TrimSpace(id), now)
	if err != nil {
		return nil, err
	}
	if session.Expired(now) {
		return nil, checkoutsession.ErrExpired
	}
	customer, err := hostedBuyer(ctx, rt, billing.CustomerID(session.CustomerID), session.Offer.Buyer)
	if err != nil {
		return nil, err
	}
	if session.Spent() {
		return hostedSpent(nil), nil
	}
	customer.ClientIP = clientIP
	option, ok := session.Option(strings.TrimSpace(input.OptionID))
	if !ok {
		return nil, checkoutsession.ErrInvalid
	}
	saved, err := hostedSavedMethods(ctx, rt, session)
	if err != nil {
		return nil, err
	}
	payment, card, err := checkoutsession.Payment(option, input, customer.VerifiedEmail, func(method string) bool {
		for _, m := range saved {
			if m.ID.String() == method && m.OptionID == option.ID {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if err := s.hostedOptionCurrent(ctx, session, option); err != nil {
		return nil, err
	}

	// A concurrent retry may start the next attempt between our advance and
	// our read, so its attempt is checked once more.
	for range 2 {
		if session.AttemptID == nil {
			break
		}
		current, err := s.getCheckoutAttempt(ctx, billing.CheckoutAttemptID(*session.AttemptID))
		if err != nil {
			return nil, fmt.Errorf("read checkout session payment: %w", err)
		}
		if !hostedRetryable(string(current.Status)) {
			return hostedPayResult(current), nil
		}
		if session, err = rt.CheckoutSessions.Advance(ctx, session, now); err != nil {
			return nil, err
		}
	}
	if session.Spent() {
		return hostedSpent(nil), nil
	}
	if session.AttemptID != nil {
		return nil, checkoutsession.ErrBusy
	}

	// A key holding other payment details is retried once, on the next
	// attempt, when its own attempt is over.
	for range 2 {
		request := hostedEngineRequest(session, customer, payment)
		created, err := s.createCheckoutAttempt(ctx, request, card)
		switch {
		case err == nil:
			return s.hostedSettle(ctx, rt, session, created)
		case errors.Is(err, billing.ErrIdempotencyKeyReused):
			if session, err = s.hostedReclaim(ctx, rt, session, request); err != nil {
				return nil, err
			}
			if session.Spent() {
				return hostedSpent(nil), nil
			}
			if card != nil {
				// The engine wiped the card: the next attempt needs it again.
				return &checkoutsession.CheckoutSessionPayResult{Status: "failed", FailureMessage: new("Enter the card again.")}, nil
			}
		case errors.Is(err, checkout.ErrCheckoutAttemptPending), errors.Is(err, checkout.ErrCheckoutProcessing):
			return nil, checkoutsession.ErrBusy
		default:
			refusal, refused := hostedRefusal(err, option.Rail)
			if !refused {
				return nil, err
			}
			if refusal.Status == "blocked" {
				// Nothing was charged and the same request is refused again.
				return refusal, nil
			}
			return s.hostedRefused(ctx, rt, session, request, refusal)
		}
	}
	return nil, checkoutsession.ErrBusy
}

// hostedSettle records the attempt a payment created and advances past it
// when it already failed terminally.
func (s *Service) hostedSettle(ctx context.Context, rt *app.Runtime, session checkoutsession.Session, created *billing.CheckoutAttempt) (*checkoutsession.CheckoutSessionPayResult, error) {
	if !created.ID.IsZero() {
		if err := rt.CheckoutSessions.Bind(ctx, session, created.ID.UUID()); err != nil {
			return nil, err
		}
	}
	result := hostedPayResult(created)
	if hostedRetryable(string(created.Status)) {
		next, err := rt.CheckoutSessions.Advance(ctx, session, s.now())
		if err != nil {
			return nil, err
		}
		if next.Spent() {
			return hostedSpent(result), nil
		}
	}
	return result, nil
}

// hostedReclaim handles an attempt whose key already holds other payment
// details, as after a refusal. Only a terminal attempt advances; a live one is
// in progress. It returns the session at its next attempt.
func (s *Service) hostedReclaim(ctx context.Context, rt *app.Runtime, session checkoutsession.Session, request billing.CreateCheckoutAttemptParams) (checkoutsession.Session, error) {
	current, found, err := s.hostedLookup(ctx, request)
	if err != nil {
		return checkoutsession.Session{}, err
	}
	if !found || !hostedRetryable(string(current.Status)) {
		return checkoutsession.Session{}, checkoutsession.ErrBusy
	}
	next, err := rt.CheckoutSessions.Advance(ctx, session, s.now())
	if err != nil {
		return checkoutsession.Session{}, err
	}
	if next.AttemptID != nil {
		return checkoutsession.Session{}, checkoutsession.ErrBusy
	}
	return next, nil
}

// hostedRefused answers a definite refusal of the current attempt. The engine
// is the authority: a live attempt for the key is adopted, and only a terminal
// or absent one lets the session advance.
func (s *Service) hostedRefused(ctx context.Context, rt *app.Runtime, session checkoutsession.Session, request billing.CreateCheckoutAttemptParams, refusal *checkoutsession.CheckoutSessionPayResult) (*checkoutsession.CheckoutSessionPayResult, error) {
	current, found, err := s.hostedLookup(ctx, request)
	if err != nil {
		return nil, err
	}
	if found && !hostedRetryable(string(current.Status)) {
		if !current.ID.IsZero() {
			if err := rt.CheckoutSessions.Bind(ctx, session, current.ID.UUID()); err != nil {
				return nil, err
			}
		}
		return hostedPayResult(current), nil
	}
	next, err := rt.CheckoutSessions.Advance(ctx, session, s.now())
	if err != nil {
		return nil, err
	}
	if next.Spent() {
		return hostedSpent(refusal), nil
	}
	return refusal, nil
}

// hostedLookup reads the attempt the session's key created, without creating,
// routing or contacting a provider.
func (s *Service) hostedLookup(ctx context.Context, request billing.CreateCheckoutAttemptParams) (*billing.CheckoutAttempt, bool, error) {
	current, err := s.lookupCheckoutAttempt(ctx, request)
	switch {
	case errors.Is(err, checkout.ErrCheckoutAttemptNotFound):
		return nil, false, nil
	case errors.Is(err, checkout.ErrCheckoutAttemptPending), errors.Is(err, billing.ErrIdempotencyKeyReused):
		// A request for the key is still running, or holds a live attempt with
		// other details: its outcome is not known.
		return nil, false, checkoutsession.ErrBusy
	case err != nil:
		return nil, false, fmt.Errorf("look up checkout session payment: %w", err)
	}
	return current, true, nil
}

// hostedOptionCurrent refuses an option whose PSP no longer sells the price.
func (s *Service) hostedOptionCurrent(ctx context.Context, session checkoutsession.Session, bound checkoutsession.Option) error {
	options, err := s.ListCheckoutOptions(ctx, billing.PriceID(session.PriceID), "", "")
	if err != nil {
		return err
	}
	for _, current := range options {
		if strings.EqualFold(current.PSP, bound.Selector) && current.PSPID == bound.PSPID &&
			strings.EqualFold(string(current.Rail), bound.Rail) && current.Mode == bound.Mode {
			return nil
		}
	}
	return errHostedOfferUnavailable
}

// hostedOffer snapshots what the session sells.
func (s *Service) hostedOffer(ctx context.Context, rt *app.Runtime, in CheckoutSessionMint) (checkoutsession.Offer, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, err
	}
	priceKey := in.PriceKey
	if in.PriceID.IsZero() == (strings.TrimSpace(priceKey) == "") {
		return checkoutsession.Offer{}, uuid.Nil, apperr.Invalidf("exactly one of price_id or price_key is required")
	}
	if (strings.TrimSpace(in.ProductKey) != "") != (strings.TrimSpace(priceKey) != "") {
		return checkoutsession.Offer{}, uuid.Nil, apperr.Invalidf("product_key and price_key must be supplied together")
	}
	var price *models.Price
	if !in.PriceID.IsZero() {
		price, err = rt.PriceService.GetByID(ctx, in.PriceID.UUID())
	} else {
		price, err = rt.PriceService.GetCurrentByProductKey(ctx, mid.UUID(), in.ProductKey, priceKey)
	}
	if db.IsNotFound(err) || (err == nil && (price == nil || price.MerchantID != mid.UUID() || !price.IsPurchasable())) {
		return checkoutsession.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, fmt.Errorf("resolve checkout session price: %w", err)
	}
	product, err := rt.ProductService.GetByID(ctx, price.ProductID)
	if db.IsNotFound(err) || (err == nil && (product == nil || !product.IsPurchasable())) {
		return checkoutsession.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, fmt.Errorf("resolve checkout session product: %w", err)
	}
	plan, err := checkoutsession.NewPlan(product.DisplayName, price.Amount, price.Currency, price.AccessDurationHours, price.AutoRenew)
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, fmt.Errorf("checkout session plan: %w", err)
	}
	offer := checkoutsession.Offer{Plan: plan, DueToday: price.Amount}
	if price.TrialUnitAmount != nil {
		offer.DueToday = *price.TrialUnitAmount
	}
	directory, err := rt.DB.Gen(ctx).GetMerchantConfigurationDirectory(ctx, mid.UUID())
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, fmt.Errorf("read merchant name: %w", err)
	}
	offer.MerchantDisplayName = strings.TrimSpace(directory.DisplayName)
	options, err := s.ListCheckoutOptions(ctx, billing.PriceID(price.ID), "", "")
	if err != nil {
		return checkoutsession.Offer{}, uuid.Nil, err
	}
	if in.Advertise != nil {
		in.Advertise(options)
	}
	for _, option := range options {
		if !checkoutsession.Drivable(option.Driver) || option.PSPID.IsZero() {
			continue
		}
		raw := make([]byte, 18)
		if _, err := rand.Read(raw); err != nil {
			return checkoutsession.Offer{}, uuid.Nil, err
		}
		offer.Options = append(offer.Options, checkoutsession.Option{
			CheckoutSessionOption: checkoutsession.CheckoutSessionOption{ID: "option_" + hex.EncodeToString(raw), PSPID: option.PSPID, Rail: string(option.Rail), Mode: option.Mode, Driver: option.Driver, PublicConfig: option.PublicConfig},
			Selector:              option.PSP,
		})
	}
	if len(offer.Options) == 0 {
		return checkoutsession.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	return offer, price.ID, nil
}

func (s *Service) hostedRuntime() (*app.Runtime, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil || rt.Config == nil || rt.CheckoutSessions == nil || rt.PriceService == nil || rt.ProductService == nil || rt.PaymentMethodService == nil {
		return nil, fmt.Errorf("billing service: checkout sessions unavailable")
	}
	return rt, nil
}

// hostedBuyer is the buyer's current identity, asked on every action. A
// customer the host says may no longer buy is ErrForbidden; a lookup failure
// stays retryable.
func hostedBuyer(ctx context.Context, rt *app.Runtime, customerID billing.CustomerID, minted checkoutsession.Buyer) (billing.CheckoutCustomerIdentity, error) {
	if rt.CheckoutCustomer == nil {
		return billing.CheckoutCustomerIdentity{ID: customerID, VerifiedEmail: minted.VerifiedEmail, Username: minted.Username}, nil
	}
	current, err := rt.CheckoutCustomer(ctx, customerID)
	if errors.Is(err, billingauth.ErrForbidden) {
		return billing.CheckoutCustomerIdentity{}, checkoutsession.ErrForbidden
	}
	if err != nil {
		return billing.CheckoutCustomerIdentity{}, apperr.New(http.StatusServiceUnavailable, "service_unavailable", "Checkout is temporarily unavailable.")
	}
	current.ID, current.ClientIP = customerID, ""
	return current, nil
}

// hostedAppOrigin is the minting app's origin, from its configuration and
// never from the request's own headers: the origin of the return URL it
// validated, else its first return origin, else its billing mount's.
func hostedAppOrigin(cfg *config.Config, successURL string) string {
	if origin, ok := config.URLOrigin(successURL); ok {
		return origin
	}
	for _, raw := range cfg.ReturnOrigins {
		if origin, ok := config.URLOrigin(raw); ok {
			return origin
		}
	}
	origin, _ := config.URLOrigin(cfg.PublicBillingBaseURL)
	return origin
}

// hostedSavedMethods are the buyer's saved cards on the session's card
// options, display data only: a PSP-held card on its PSP's option, a card a
// custodian holds on the first card option of its rail.
func hostedSavedMethods(ctx context.Context, rt *app.Runtime, session checkoutsession.Session) ([]checkoutsession.CheckoutSessionSavedMethod, error) {
	var cards []checkoutsession.Option
	for _, option := range session.Offer.Options {
		if checkoutsession.TakesCards(option.Driver) {
			cards = append(cards, option)
		}
	}
	out := []checkoutsession.CheckoutSessionSavedMethod{}
	if len(cards) == 0 {
		return out, nil
	}
	page, err := rt.PaymentMethodService.ListPage(ctx, session.CustomerID, billing.PageRequest{Limit: billing.MaxPageLimit})
	if err != nil {
		return nil, fmt.Errorf("list saved payment methods: %w", err)
	}
	for _, method := range page.Items {
		for _, option := range cards {
			if option.PSPID.IsZero() || !strings.EqualFold(option.Rail, string(method.Rail)) || !method.ChargeableOn(option.PSPID.UUID()) {
				continue
			}
			out = append(out, checkoutsession.CheckoutSessionSavedMethod{ID: billing.PaymentMethodID(method.ID), OptionID: option.ID, Rail: option.Rail, Card: method.Card.Details()})
			break
		}
	}
	return out, nil
}

func hostedEngineRequest(session checkoutsession.Session, customer billing.CheckoutCustomerIdentity, payment billing.CheckoutPaymentOptions) billing.CreateCheckoutAttemptParams {
	return billing.CreateCheckoutAttemptParams{
		Customer:       customer,
		PriceID:        billing.PriceID(session.PriceID),
		IdempotencyKey: session.AttemptKey(),
		SuccessURL:     session.SuccessURL,
		CancelURL:      session.SuccessURL,
		Metadata:       map[string]string{"source": "checkout_session"},
		PaymentOptions: payment,
	}
}

// hostedRefusal classifies a definite refusal: failed when the buyer can
// answer with another instrument, blocked when the purchase itself is refused
// (the buyer already holds this membership).
func hostedRefusal(err error, rail string) (*checkoutsession.CheckoutSessionPayResult, bool) {
	out := &checkoutsession.CheckoutSessionPayResult{Status: "failed"}
	var method *paymentmethods.PaymentMethodError
	var funds *recurring.InsufficientUSDCError
	var conflict *apperr.Error
	switch {
	case errors.As(err, &conflict) && conflict.Status == http.StatusConflict && conflict.Code == api.CodeResourceConflict:
		out.Status, out.FailureMessage = "blocked", new(conflict.Message)
	case errors.Is(err, checkout.ErrCheckoutAttemptConflict):
		out.Status, out.FailureMessage = "blocked", new("This purchase is not available for your account.")
	case errors.As(err, &method):
		reason := method.Reason
		if reason == "" {
			if method.Rail != "" {
				rail = method.Rail
			}
			reason = decline.Classify(rail, strings.TrimSpace(method.LocalizationID)).Reason
		}
		if decline.ProviderFault(reason) {
			out.FailureMessage = new("The payment processor could not complete this payment. Try again later.")
			return out, true
		}
		failure := reason.Failure()
		out.Failure, out.FailureMessage = &failure, new(failure.Message)
	case errors.Is(err, checkout.ErrPaymentMethodStale):
		out.FailureMessage = new("That saved payment method can no longer be charged. Enter the card again.")
	case errors.As(err, &funds):
		out.FailureMessage = new(funds.Error())
	default:
		return nil, false
	}
	return out, true
}

// hostedSpent answers a session whose last attempt failed: it is over.
func hostedSpent(result *checkoutsession.CheckoutSessionPayResult) *checkoutsession.CheckoutSessionPayResult {
	if result == nil {
		result = &checkoutsession.CheckoutSessionPayResult{}
	}
	result.Status = "expired"
	return result
}

// hostedOver reports a session that can take no further payment: spent, or
// past its time with no payment that may still settle.
func hostedOver(session checkoutsession.Session, status string, now time.Time) bool {
	switch status {
	case "created":
		return session.Spent() || (session.AttemptID == nil && session.Expired(now))
	case "failed", "canceled":
		return session.Spent() || session.Expired(now)
	}
	return false
}

func hostedPayResult(state *billing.CheckoutAttempt) *checkoutsession.CheckoutSessionPayResult {
	status := hostedStatus(string(state.Status))
	return &checkoutsession.CheckoutSessionPayResult{
		Status: status, NextAction: hostedNextAction(state), Operation: hostedOperation(state),
		PaymentID: state.PaymentID, SubscriptionID: state.SubscriptionID,
		FailureMessage: hostedFailureMessage(status), Failure: state.Failure,
	}
}

func hostedStatus(status string) string {
	switch status = strings.ToLower(strings.TrimSpace(status)); status {
	case "created", "requires_action", "processing", "succeeded", "failed", "expired", "canceled":
		return status
	}
	return "blocked"
}

func hostedRetryable(status string) bool {
	switch hostedStatus(status) {
	case "failed", "expired", "canceled":
		return true
	}
	return false
}

func hostedFailureMessage(status string) *string {
	if status == "blocked" {
		return new("Checkout is unavailable.")
	}
	return nil
}

// hostedNextAction is the attempt's next step as the payment page may take
// it: an https redirect without credentials, or a solana: Solana Pay link.
func hostedNextAction(state *billing.CheckoutAttempt) *billing.NextAction {
	next := state.NextAction
	if next == nil || next.URL == nil {
		return nil
	}
	raw := strings.TrimSpace(*next.URL)
	switch next.Type {
	case "redirect_to_url":
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return nil
		}
		link := parsed.String()
		return &billing.NextAction{Type: next.Type, URL: &link}
	case "solana_pay":
		if !strings.HasPrefix(raw, "solana:") {
			return nil
		}
		return &billing.NextAction{Type: next.Type, URL: &raw}
	}
	return nil
}

// hostedOperation is the card payment the buyer authenticates, while the
// attempt waits for it.
func hostedOperation(state *billing.CheckoutAttempt) *billing.PaymentOperation {
	if state.Status != billing.CheckoutAttemptRequiresAction {
		return nil
	}
	return state.Operation
}
