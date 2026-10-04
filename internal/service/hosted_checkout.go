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
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/hostedcheckout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/apperr"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Hosted checkout (#1124). A session is minted for one signed-in customer and
// one price; its id then reads and pays it with no other credential. Paying
// runs the engine's own checkout (createCheckoutSessionForCustomer with
// Confirm), one idempotency key per attempt.

var errHostedOfferUnavailable = apperr.New(http.StatusUnprocessableEntity, "checkout_offer_unavailable", "This purchase is not available.")

// HostedCheckoutMint is a mint request. Advertise attaches the browser driver
// and public values to the price's options (the HTTP layer's checkout-config
// projection); an option the page cannot drive is not offered.
type HostedCheckoutMint struct {
	billing.CreateHostedCheckoutSessionRequest
	Advertise func([]billing.CheckoutRailOption)
}

// CreateHostedCheckoutSession mints a session for the customer and price.
func (s *Service) CreateHostedCheckoutSession(ctx context.Context, in HostedCheckoutMint) (*billing.HostedCheckoutSessionLink, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.hostedRuntime()
	if err != nil {
		return nil, err
	}
	customerID, err := billing.ParseCustomerID(strings.TrimSpace(in.Customer.ID))
	if err != nil || customerID.IsZero() {
		return nil, apperr.Invalidf("valid customer id required")
	}
	if _, err := hostedBuyer(ctx, rt, customerID.String(), hostedcheckout.Buyer{}); err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(in.SuccessURL)
	if successURL != "" && !rt.Config.ReturnURLAllowed(successURL) {
		return nil, apperr.Invalidf("success_url origin is not allowed").WithParam("success_url")
	}
	offer, priceID, err := s.hostedOffer(ctx, rt, in)
	if err != nil {
		return nil, err
	}
	offer.Buyer = hostedcheckout.Buyer{VerifiedEmail: strings.TrimSpace(in.Customer.VerifiedEmail), Username: strings.TrimSpace(in.Customer.Username)}
	id, err := hostedcheckout.NewID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	session := hostedcheckout.Session{
		CustomerID: customerID.UUID(), PriceID: priceID, Offer: offer, SuccessURL: successURL,
		Origin: hostedAppOrigin(rt.Config, successURL), ExpiresAt: now.Add(hostedcheckout.SessionTTL),
	}
	if err := rt.HostedCheckout.Create(ctx, id, session, now); err != nil {
		return nil, fmt.Errorf("create hosted checkout session: %w", err)
	}
	link := &billing.HostedCheckoutSessionLink{ID: id, ExpiresAt: session.ExpiresAt}
	if page := strings.TrimSpace(rt.Config.HostedCheckout().PageURL); page != "" {
		link.URL = page + "#" + id
	}
	return link, nil
}

// GetHostedCheckoutSession is the session document for its id.
func (s *Service) GetHostedCheckoutSession(ctx context.Context, id string) (*billing.HostedCheckoutSession, error) {
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
	session, err := rt.HostedCheckout.Get(ctx, strings.TrimSpace(id), now)
	if err != nil {
		return nil, err
	}
	if _, err := hostedBuyer(ctx, rt, session.CustomerID.String(), session.Offer.Buyer); err != nil {
		return nil, err
	}
	state := &billing.CheckoutSession{Status: "created"}
	if session.EngineSessionID != nil {
		if state, err = s.GetCheckoutSession(ctx, session.CustomerID.String(), *session.EngineSessionID); err != nil {
			return nil, fmt.Errorf("read hosted checkout payment: %w", err)
		}
	}
	status := hostedStatus(state.Status)
	if hostedOver(session, status, now) {
		status = "expired"
	}
	due, tax := session.Offer.DueToday, int64(0)
	out := &billing.HostedCheckoutSession{
		ID: session.ID, Status: status,
		Merchant:  billing.HostedCheckoutMerchant{DisplayName: session.Offer.MerchantDisplayName},
		Plan:      session.Offer.Plan,
		LineItems: []billing.HostedCheckoutLineItem{{Label: session.Offer.Plan.DisplayName, Amount: due}},
		Tax:       &tax, DueToday: &due,
		Rails:          make([]billing.HostedCheckoutRail, 0, len(session.Offer.Options)),
		TransactionURL: hostedTransactionURL(state),
		FailureMessage: hostedFailureMessage(status),
		Failure:        state.Failure,
		SuccessURL:     session.SuccessURL,
		ExpiresAt:      session.ExpiresAt,
	}
	if state.PaymentID != nil {
		out.PaymentID = *state.PaymentID
	}
	if state.SubscriptionID != nil {
		out.SubscriptionID = *state.SubscriptionID
	}
	for _, option := range session.Offer.Options {
		out.Rails = append(out.Rails, option.HostedCheckoutRail)
	}
	if rt.Config.CheckoutEmbedAllowed(session.Origin) {
		out.EmbedOrigin = session.Origin
	}
	if out.SavedMethods, err = hostedSavedMethods(ctx, rt, session); err != nil {
		return nil, err
	}
	return out, nil
}

// PayHostedCheckoutSession starts or resumes the session's current attempt.
// Every submission of one attempt sends the engine the same idempotency key,
// so repeated and concurrent submissions charge at most once. The attempt
// advances only after the engine reports it terminally failed; an ambiguous
// error keeps it, and the next submission replays the same key.
func (s *Service) PayHostedCheckoutSession(ctx context.Context, id string, input billing.HostedCheckoutPayRequest, clientIP string) (*billing.HostedCheckoutPayResult, error) {
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
	session, err := rt.HostedCheckout.Get(ctx, strings.TrimSpace(id), now)
	if err != nil {
		return nil, err
	}
	if session.Expired(now) {
		return nil, hostedcheckout.ErrExpired
	}
	customer, err := hostedBuyer(ctx, rt, session.CustomerID.String(), session.Offer.Buyer)
	if err != nil {
		return nil, err
	}
	if session.Spent() {
		return hostedSpent(nil), nil
	}
	customer.ClientIP = clientIP
	option, ok := session.Option(strings.TrimSpace(input.OptionID))
	if !ok {
		return nil, hostedcheckout.ErrInvalid
	}
	saved, err := hostedSavedMethods(ctx, rt, session)
	if err != nil {
		return nil, err
	}
	payment, err := hostedcheckout.Payment(option, input, customer.VerifiedEmail, func(method string) bool {
		for _, m := range saved {
			if m.ID == method && m.OptionID == option.ID {
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
	// our read, so its session is checked once more.
	for range 2 {
		if session.EngineSessionID == nil {
			break
		}
		current, err := s.GetCheckoutSession(ctx, session.CustomerID.String(), *session.EngineSessionID)
		if err != nil {
			return nil, fmt.Errorf("read hosted checkout payment: %w", err)
		}
		if !hostedRetryable(current.Status) {
			return hostedPayResult(current), nil
		}
		if session, err = rt.HostedCheckout.Advance(ctx, session, now); err != nil {
			return nil, err
		}
	}
	if session.Spent() {
		return hostedSpent(nil), nil
	}
	if session.EngineSessionID != nil {
		return nil, hostedcheckout.ErrBusy
	}

	// A key holding other payment details is retried once, on the next
	// attempt, when its own attempt is over.
	for range 2 {
		request := hostedEngineRequest(session, payment)
		created, err := s.createCheckoutSessionForCustomer(ctx, customer, request, "", "", "")
		switch {
		case err == nil:
			return s.hostedSettle(ctx, rt, session, created)
		case errors.Is(err, billing.ErrIdempotencyKeyReused):
			if session, err = s.hostedReclaim(ctx, rt, session, customer, request); err != nil {
				return nil, err
			}
			if session.Spent() {
				return hostedSpent(nil), nil
			}
		case errors.Is(err, checkout.ErrCheckoutSessionPending), errors.Is(err, checkout.ErrCheckoutProcessing):
			return nil, hostedcheckout.ErrBusy
		default:
			refusal, refused := hostedRefusal(err, option.Rail)
			if !refused {
				return nil, err
			}
			if refusal.Status == "blocked" {
				// Nothing was charged and the same request is refused again.
				return refusal, nil
			}
			return s.hostedRefused(ctx, rt, session, customer, request, refusal)
		}
	}
	return nil, hostedcheckout.ErrBusy
}

// hostedSettle records the session an attempt created and advances past it
// when it already failed terminally.
func (s *Service) hostedSettle(ctx context.Context, rt *app.Runtime, session hostedcheckout.Session, created *billing.CheckoutSession) (*billing.HostedCheckoutPayResult, error) {
	if id, err := billing.ParseCheckoutSessionID(created.ID); err == nil && !id.IsZero() {
		if err := rt.HostedCheckout.Bind(ctx, session, id.UUID()); err != nil {
			return nil, err
		}
	}
	result := hostedPayResult(created)
	if hostedRetryable(created.Status) {
		next, err := rt.HostedCheckout.Advance(ctx, session, s.now())
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
func (s *Service) hostedReclaim(ctx context.Context, rt *app.Runtime, session hostedcheckout.Session, customer billing.CheckoutCustomerIdentity, request billing.CreateCheckoutSessionRequest) (hostedcheckout.Session, error) {
	current, found, err := s.hostedLookup(ctx, customer, request)
	if err != nil {
		return hostedcheckout.Session{}, err
	}
	if !found || !hostedRetryable(current.Status) {
		return hostedcheckout.Session{}, hostedcheckout.ErrBusy
	}
	next, err := rt.HostedCheckout.Advance(ctx, session, s.now())
	if err != nil {
		return hostedcheckout.Session{}, err
	}
	if next.EngineSessionID != nil {
		return hostedcheckout.Session{}, hostedcheckout.ErrBusy
	}
	return next, nil
}

// hostedRefused answers a definite refusal of the current attempt. The engine
// is the authority: a live session for the key is adopted, and only a terminal
// or absent one lets the attempt advance.
func (s *Service) hostedRefused(ctx context.Context, rt *app.Runtime, session hostedcheckout.Session, customer billing.CheckoutCustomerIdentity, request billing.CreateCheckoutSessionRequest, refusal *billing.HostedCheckoutPayResult) (*billing.HostedCheckoutPayResult, error) {
	current, found, err := s.hostedLookup(ctx, customer, request)
	if err != nil {
		return nil, err
	}
	if found && !hostedRetryable(current.Status) {
		if id, err := billing.ParseCheckoutSessionID(current.ID); err == nil && !id.IsZero() {
			if err := rt.HostedCheckout.Bind(ctx, session, id.UUID()); err != nil {
				return nil, err
			}
		}
		return hostedPayResult(current), nil
	}
	next, err := rt.HostedCheckout.Advance(ctx, session, s.now())
	if err != nil {
		return nil, err
	}
	if next.Spent() {
		return hostedSpent(refusal), nil
	}
	return refusal, nil
}

// hostedLookup reads the engine session the attempt's key created, without
// creating, routing or contacting a provider.
func (s *Service) hostedLookup(ctx context.Context, customer billing.CheckoutCustomerIdentity, request billing.CreateCheckoutSessionRequest) (*billing.CheckoutSession, bool, error) {
	user, err := checkoutUserIdentity(customer)
	if err != nil {
		return nil, false, err
	}
	current, err := s.lookupCheckoutSession(ctx, user, request)
	switch {
	case errors.Is(err, checkout.ErrCheckoutSessionNotFound):
		return nil, false, nil
	case errors.Is(err, checkout.ErrCheckoutSessionPending), errors.Is(err, billing.ErrIdempotencyKeyReused):
		// A request for the key is still running, or holds a live session with
		// other details: its outcome is not known.
		return nil, false, hostedcheckout.ErrBusy
	case err != nil:
		return nil, false, fmt.Errorf("look up hosted checkout payment: %w", err)
	}
	return current, true, nil
}

// hostedOptionCurrent refuses an option whose PSP no longer sells the price.
func (s *Service) hostedOptionCurrent(ctx context.Context, session hostedcheckout.Session, bound hostedcheckout.Option) error {
	options, err := s.ListCheckoutRailOptions(ctx, billing.PriceID(session.PriceID).String(), "")
	if err != nil {
		return err
	}
	for _, current := range options {
		if strings.EqualFold(current.Selector, bound.Selector) && current.PSPID == bound.PSPID &&
			strings.EqualFold(current.Rail, bound.Rail) && current.Mode == bound.Mode {
			return nil
		}
	}
	return errHostedOfferUnavailable
}

// hostedOffer snapshots what the session sells.
func (s *Service) hostedOffer(ctx context.Context, rt *app.Runtime, in HostedCheckoutMint) (hostedcheckout.Offer, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return hostedcheckout.Offer{}, uuid.Nil, err
	}
	priceID, priceKey := strings.TrimSpace(in.PriceID), in.PriceKey
	if (priceID == "") == (strings.TrimSpace(priceKey) == "") {
		return hostedcheckout.Offer{}, uuid.Nil, apperr.Invalidf("exactly one of price_id or price_key is required")
	}
	var price *models.Price
	if priceID != "" {
		parsed, perr := billing.ParsePriceID(priceID)
		if perr != nil || parsed.IsZero() {
			return hostedcheckout.Offer{}, uuid.Nil, apperr.Invalidf("price_id must be a valid price ID; use price_key for an opaque key").WithParam("price_id")
		}
		price, err = rt.PriceService.GetByID(ctx, parsed.UUID())
	} else {
		price, err = rt.PriceService.GetCurrentByKey(ctx, mid.UUID(), priceKey)
	}
	if db.IsNotFound(err) || (err == nil && (price == nil || price.MerchantID != mid.UUID() || !price.IsPurchasable())) {
		return hostedcheckout.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	if err != nil {
		return hostedcheckout.Offer{}, uuid.Nil, fmt.Errorf("resolve hosted checkout price: %w", err)
	}
	product, err := rt.ProductService.GetByID(ctx, price.ProductID)
	if db.IsNotFound(err) || (err == nil && (product == nil || !product.IsPurchasable())) {
		return hostedcheckout.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	if err != nil {
		return hostedcheckout.Offer{}, uuid.Nil, fmt.Errorf("resolve hosted checkout product: %w", err)
	}
	units, ok := billing.LookupCurrency(price.Currency)
	if !ok {
		return hostedcheckout.Offer{}, uuid.Nil, fmt.Errorf("hosted checkout: price currency %q is not registered", price.Currency)
	}
	offer := hostedcheckout.Offer{
		Plan: billing.HostedCheckoutPlan{
			DisplayName: product.DisplayName, UnitAmount: price.Amount, Currency: units.Code, UnitDecimals: units.Decimals,
			PeriodHours: price.AccessDurationHours, AutomaticallyRenews: price.AutoRenew,
		},
		DueToday: price.Amount,
	}
	if price.TrialUnitAmount != nil {
		offer.DueToday = *price.TrialUnitAmount
	}
	directory, err := rt.DB.Gen(ctx).GetMerchantConfigurationDirectory(ctx, mid.UUID())
	if err != nil {
		return hostedcheckout.Offer{}, uuid.Nil, fmt.Errorf("read merchant name: %w", err)
	}
	offer.MerchantDisplayName = strings.TrimSpace(directory.DisplayName)
	options, err := s.ListCheckoutRailOptions(ctx, billing.PriceID(price.ID).String(), "")
	if err != nil {
		return hostedcheckout.Offer{}, uuid.Nil, err
	}
	if in.Advertise != nil {
		in.Advertise(options)
	}
	for _, option := range options {
		if !hostedcheckout.Drivable(option.Driver) || option.PSPID == "" {
			continue
		}
		raw := make([]byte, 18)
		if _, err := rand.Read(raw); err != nil {
			return hostedcheckout.Offer{}, uuid.Nil, err
		}
		offer.Options = append(offer.Options, hostedcheckout.Option{
			HostedCheckoutRail: billing.HostedCheckoutRail{ID: "option_" + hex.EncodeToString(raw), Rail: option.Rail, Mode: option.Mode, Driver: option.Driver, PublicConfig: option.PublicConfig},
			Selector:           option.Selector, PSPID: option.PSPID,
		})
	}
	if len(offer.Options) == 0 {
		return hostedcheckout.Offer{}, uuid.Nil, errHostedOfferUnavailable
	}
	return offer, price.ID, nil
}

func (s *Service) hostedRuntime() (*app.Runtime, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil || rt.Config == nil || rt.HostedCheckout == nil || rt.PriceService == nil || rt.ProductService == nil || rt.PaymentMethodService == nil {
		return nil, fmt.Errorf("billing service: hosted checkout unavailable")
	}
	return rt, nil
}

// hostedBuyer is the buyer's current identity, asked on every action. A
// customer the host says may no longer buy is ErrForbidden; a lookup failure
// stays retryable.
func hostedBuyer(ctx context.Context, rt *app.Runtime, customerID string, minted hostedcheckout.Buyer) (billing.CheckoutCustomerIdentity, error) {
	if rt.CheckoutCustomer == nil {
		return billing.CheckoutCustomerIdentity{ID: customerID, VerifiedEmail: minted.VerifiedEmail, Username: minted.Username}, nil
	}
	current, err := rt.CheckoutCustomer(ctx, customerID)
	if errors.Is(err, billingauth.ErrForbidden) {
		return billing.CheckoutCustomerIdentity{}, hostedcheckout.ErrForbidden
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
// options, display data only.
func hostedSavedMethods(ctx context.Context, rt *app.Runtime, session hostedcheckout.Session) ([]billing.HostedCheckoutSavedMethod, error) {
	byPSP := map[string]hostedcheckout.Option{}
	for _, option := range session.Offer.Options {
		if option.Driver == "collect_js" {
			byPSP[option.PSPID] = option
		}
	}
	if len(byPSP) == 0 {
		return nil, nil
	}
	methods, _, err := rt.PaymentMethodService.ListByUserID(ctx, session.CustomerID.String(), 100, 0)
	if err != nil {
		return nil, fmt.Errorf("list saved payment methods: %w", err)
	}
	var out []billing.HostedCheckoutSavedMethod
	for _, method := range methods {
		option, ok := byPSP[method.PspID.String()]
		if !ok || !strings.EqualFold(option.Rail, string(method.Rail)) {
			continue
		}
		item := billing.HostedCheckoutSavedMethod{ID: billing.PaymentMethodID(method.ID).String(), OptionID: option.ID, Rail: option.Rail}
		if method.CardType != nil {
			item.Brand = strings.TrimSpace(*method.CardType)
		}
		if method.LastFour != nil {
			item.LastFour = strings.TrimSpace(*method.LastFour)
		}
		if method.ExpiryDate != nil {
			if month, year, err := sharedformat.ParseExpiry(*method.ExpiryDate); err == nil {
				item.ExpMonth, item.ExpYear = &month, &year
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func hostedEngineRequest(session hostedcheckout.Session, payment billing.CheckoutPaymentOptions) billing.CreateCheckoutSessionRequest {
	return billing.CreateCheckoutSessionRequest{
		PriceID:        billing.PriceID(session.PriceID).String(),
		IdempotencyKey: session.AttemptKey(),
		SuccessURL:     session.SuccessURL,
		CancelURL:      session.SuccessURL,
		// The buyer's pay click accepts the displayed price.
		Confirm:        true,
		Metadata:       map[string]string{"source": "hosted_checkout"},
		PaymentOptions: payment,
	}
}

// hostedRefusal classifies a definite refusal: failed when the buyer can
// answer with another instrument, blocked when the purchase itself is refused
// (the buyer already holds this membership).
func hostedRefusal(err error, rail string) (*billing.HostedCheckoutPayResult, bool) {
	out := &billing.HostedCheckoutPayResult{Status: "failed"}
	var method *paymentmethods.PaymentMethodError
	var funds *recurring.InsufficientUSDCError
	var conflict *apperr.Error
	switch {
	case errors.As(err, &conflict) && conflict.Status == http.StatusConflict && conflict.Code == api.CodeResourceConflict:
		out.Status, out.FailureMessage = "blocked", conflict.Message
	case errors.Is(err, checkout.ErrCheckoutSessionConflict):
		out.Status, out.FailureMessage = "blocked", "This purchase is not available for your account."
	case errors.As(err, &method):
		reason := method.Reason
		if reason == "" {
			if method.Rail != "" {
				rail = method.Rail
			}
			reason = decline.Classify(rail, strings.TrimSpace(method.LocalizationID)).Reason
		}
		if decline.ProviderFault(reason) {
			out.FailureMessage = "The payment processor could not complete this payment. Try again later."
			return out, true
		}
		failure := reason.Failure()
		out.Failure, out.FailureMessage = &failure, failure.Message
	case errors.Is(err, checkout.ErrPaymentMethodStale):
		out.FailureMessage = "That saved payment method can no longer be charged. Enter the card again."
	case errors.As(err, &funds):
		out.FailureMessage = funds.Error()
	default:
		return nil, false
	}
	return out, true
}

// hostedSpent answers a session whose last attempt failed: it is over.
func hostedSpent(result *billing.HostedCheckoutPayResult) *billing.HostedCheckoutPayResult {
	if result == nil {
		result = &billing.HostedCheckoutPayResult{}
	}
	result.Status = "expired"
	return result
}

// hostedOver reports a session that can take no further payment: spent, or
// past its time with no payment that may still settle.
func hostedOver(session hostedcheckout.Session, status string, now time.Time) bool {
	switch status {
	case "created":
		return session.Spent() || (session.EngineSessionID == nil && session.Expired(now))
	case "failed", "canceled":
		return session.Spent() || session.Expired(now)
	}
	return false
}

func hostedPayResult(state *billing.CheckoutSession) *billing.HostedCheckoutPayResult {
	status := hostedStatus(state.Status)
	out := &billing.HostedCheckoutPayResult{
		Status: status, RedirectURL: hostedRedirectURL(state), TransactionURL: hostedTransactionURL(state),
		FailureMessage: hostedFailureMessage(status), Failure: state.Failure,
	}
	if state.PaymentID != nil {
		out.PaymentID = *state.PaymentID
	}
	if state.SubscriptionID != nil {
		out.SubscriptionID = *state.SubscriptionID
	}
	return out
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

func hostedFailureMessage(status string) string {
	if status == "blocked" {
		return "Checkout is unavailable."
	}
	return ""
}

func hostedRedirectURL(state *billing.CheckoutSession) string {
	raw, _ := state.RailData["redirect_url"].(string)
	if state.URL != nil && strings.TrimSpace(*state.URL) != "" {
		raw = *state.URL
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return parsed.String()
}

func hostedTransactionURL(state *billing.CheckoutSession) string {
	for _, key := range []string{"solana_pay_url", "transaction_url"} {
		if raw, _ := state.RailData[key].(string); strings.HasPrefix(strings.TrimSpace(raw), "solana:") {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}
