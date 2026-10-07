package service

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// ListCheckoutOptions returns the ready ways to sell a price, in routing
// order. It does not probe providers or write.
func (s *Service) ListCheckoutOptions(ctx context.Context, priceID billing.PriceID, productKey, priceKey string) ([]billing.CheckoutOption, error) {
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	id := ""
	if !priceID.IsZero() {
		id = priceID.String()
	}
	var options []checkout.CheckoutOption
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		var listErr error
		options, listErr = engine.ListCheckoutOptions(scoped, id, productKey, priceKey)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("list checkout options: %w", err)
	}
	out := make([]billing.CheckoutOption, 0, len(options))
	for _, option := range options {
		item := billing.CheckoutOption{PSP: option.Selector, Rail: billing.Rail(option.Rail), Mode: option.Mode}
		item.PSPID = billing.PSPID(option.PSPID)
		if option.Token != "" {
			item.PublicConfig = map[string]string{"token_symbol": option.Token}
		}
		out = append(out, item)
	}
	return out, nil
}

// CreateCheckoutAttempt charges one price for a customer, relaying that
// customer's pay action: a recurring price is enrolled and charged now.
func (s *Service) CreateCheckoutAttempt(ctx context.Context, req billing.CreateCheckoutAttemptParams) (*billing.CheckoutAttempt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.createCheckoutAttempt(ctx, req, nil)
}

// createCheckoutAttempt runs the engine's checkout. card is a card the
// checkout session's page entered; the engine wipes it.
func (s *Service) createCheckoutAttempt(ctx context.Context, req billing.CreateCheckoutAttemptParams, card *cardguard.Card) (*billing.CheckoutAttempt, error) {
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	user, err := checkoutUserIdentity(req.Customer)
	if err != nil {
		return nil, err
	}
	in, err := checkoutCreateRequest(req, card)
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutAttemptResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		mid, err := merchant.Require(scoped)
		if err != nil {
			return err
		}
		// The host relays its customer's pay action on the displayed price.
		in.Acceptance = &billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid, SubjectID: user.ID, Issuer: "openrails:merchant-checkout", Email: strings.TrimSpace(req.Customer.VerifiedEmail)}
		var createErr error
		resp, createErr = engine.CreateSession(scoped, in, user)
		return createErr
	})
	if err != nil {
		return nil, err
	}
	return checkoutAttemptFromResponse(resp, req.Customer.ID), nil
}

// lookupCheckoutAttempt reads the attempt an idempotency key created, without
// creating, routing or contacting a provider.
func (s *Service) lookupCheckoutAttempt(ctx context.Context, req billing.CreateCheckoutAttemptParams) (*billing.CheckoutAttempt, error) {
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	user, err := checkoutUserIdentity(req.Customer)
	if err != nil {
		return nil, err
	}
	in, err := checkoutCreateRequest(req, nil)
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var resp *checkout.CheckoutAttemptResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		resp, err = engine.LookupSession(scoped, in, user)
		return err
	})
	if err != nil {
		return nil, err
	}
	return checkoutAttemptFromResponse(resp, req.Customer.ID), nil
}

// CheckoutAttemptOwner is the customer an attempt charges, for scoping the
// caller before the attempt is read or confirmed.
func (s *Service) CheckoutAttemptOwner(ctx context.Context, id billing.CheckoutAttemptID) (billing.CustomerID, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.CustomerID{}, err
	}
	defer release()
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return billing.CustomerID{}, err
	}
	rt, err := s.runtime()
	if err != nil {
		return billing.CustomerID{}, err
	}
	var owner checkout.AttemptOwner
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		owner, err = engine.Owner(scoped, id.UUID())
		return err
	})
	return billing.CustomerID(owner.CustomerID), err
}

// GetCheckoutAttempt reads one attempt.
func (s *Service) GetCheckoutAttempt(ctx context.Context, id billing.CheckoutAttemptID) (*billing.CheckoutAttempt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.getCheckoutAttempt(ctx, id)
}

func (s *Service) getCheckoutAttempt(ctx context.Context, id billing.CheckoutAttemptID) (*billing.CheckoutAttempt, error) {
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutAttemptResponse
	var owner checkout.AttemptOwner
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		if owner, err = engine.Owner(scoped, id.UUID()); err != nil {
			return err
		}
		resp, err = engine.GetSession(scoped, id.UUID(), &checkout.UserIdentity{ID: owner.CustomerID.String()})
		return err
	})
	if err != nil {
		return nil, err
	}
	return checkoutAttemptFromResponse(resp, billing.CustomerID(owner.CustomerID)), nil
}

// ConfirmCheckoutAttempt completes a Solana attempt with the signature of the
// transaction the buyer's wallet signed.
func (s *Service) ConfirmCheckoutAttempt(ctx context.Context, id billing.CheckoutAttemptID, req billing.ConfirmCheckoutAttemptParams) (*billing.CheckoutAttempt, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	engine, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutAttemptResponse
	var owner checkout.AttemptOwner
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		if owner, err = engine.Owner(scoped, id.UUID()); err != nil {
			return err
		}
		in := &checkout.CheckoutAttemptConfirmRequest{Payment: checkout.CheckoutAttemptConfirmPayment{Rail: owner.Rail, Signature: req.Signature, Wallet: req.Wallet}}
		resp, err = engine.ConfirmSession(scoped, id.UUID(), in, &checkout.UserIdentity{ID: owner.CustomerID.String()})
		return err
	})
	if err != nil {
		return nil, err
	}
	return checkoutAttemptFromResponse(resp, billing.CustomerID(owner.CustomerID)), nil
}

func checkoutUserIdentity(customer billing.CheckoutCustomerIdentity) (*checkout.UserIdentity, error) {
	if customer.ID.IsZero() {
		return nil, fmt.Errorf("%w: customer.id is required", checkout.ErrCheckoutAttemptValidation)
	}
	var email *string
	if verified := strings.TrimSpace(customer.VerifiedEmail); verified != "" {
		email = &verified
	}
	clientIP := ""
	if raw := strings.TrimSpace(customer.ClientIP); raw != "" {
		addr, err := netip.ParseAddr(raw)
		if err != nil || addr.Zone() != "" || addr.IsUnspecified() {
			return nil, fmt.Errorf("%w: customer.client_ip must be the customer's IP address", checkout.ErrCheckoutAttemptValidation)
		}
		clientIP = addr.Unmap().String()
	}
	return &checkout.UserIdentity{ID: customer.ID.String(), Email: email, Username: strings.TrimSpace(customer.Username), ClientIP: clientIP}, nil
}

func checkoutCreateRequest(req billing.CreateCheckoutAttemptParams, card *cardguard.Card) (*checkout.CheckoutAttemptCreateRequest, error) {
	priceID := ""
	if !req.PriceID.IsZero() {
		priceID = req.PriceID.String()
	}
	payment := req.PaymentOptions
	return &checkout.CheckoutAttemptCreateRequest{
		PriceID:  priceID,
		PriceKey: req.PriceKey, ProductKey: req.ProductKey,
		Entitlement:    req.Entitlement,
		OfferKind:      req.OfferKind,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		SuccessURL:     req.SuccessURL,
		CancelURL:      req.CancelURL,
		Payment:        checkoutAttemptPayment(payment, card),
	}, nil
}

// checkoutAttemptPayment is the engine's flat payment request: a PSP named by
// its key, and the billing details as the providers take them.
func checkoutAttemptPayment(payment billing.CheckoutPaymentOptions, card *cardguard.Card) checkout.CheckoutAttemptPaymentRequest {
	out := checkout.CheckoutAttemptPaymentRequest{
		Rail: payment.PSP, PaymentToken: payment.PaymentToken, Card: card,
		TokenSymbol: payment.TokenSymbol, Flow: payment.Flow, Wallet: payment.Wallet,
	}
	if !payment.PaymentMethodID.IsZero() {
		out.PaymentMethodID = payment.PaymentMethodID.String()
	}
	text := func(v *string) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(*v)
	}
	if d := payment.BillingDetails; d != nil {
		out.NameOnCard, out.Email, out.Phone = text(d.Name), text(d.Email), text(d.Phone)
		if a := d.Address; a != nil {
			out.Address1, out.Address2, out.City, out.State = text(a.Line1), text(a.Line2), text(a.City), text(a.State)
			out.Zip, out.Country = text(a.PostalCode), strings.ToUpper(text(a.Country))
		}
	}
	return out
}

// checkoutAttemptFromResponse is the merchant's view of an engine answer: rail
// steps become one NextAction.
func checkoutAttemptFromResponse(resp *checkout.CheckoutAttemptResponse, customer billing.CustomerID) *billing.CheckoutAttempt {
	out := &billing.CheckoutAttempt{
		ID: resp.ID, CustomerID: customer,
		Status: billing.CheckoutAttemptStatus(resp.Status), Mode: resp.Mode,
		PriceID: resp.PriceID, Amount: resp.Amount, Currency: resp.Currency,
		PaymentID: resp.PaymentID, SubscriptionID: resp.SubscriptionID, PaymentMethodID: resp.PaymentMethodID,
		NextAction: checkoutNextAction(resp), Operation: resp.Operation, Failure: resp.Failure,
		ExpiresAt: resp.ExpiresAt, CreatedAt: resp.CreatedAt, Metadata: resp.Metadata,
	}
	return out
}

// checkoutNextAction is the step an attempt awaits, null when none.
func checkoutNextAction(resp *checkout.CheckoutAttemptResponse) *billing.NextAction {
	if resp.Status != string(billing.CheckoutAttemptRequiresAction) {
		return nil
	}
	if next := resp.NextAction; next != nil && next.Type == "solana_sign_transactions" && len(next.Transactions) > 0 {
		return &billing.NextAction{Type: "solana_sign_transactions", Transactions: next.Transactions}
	}
	redirect := strings.TrimSpace(resp.URL)
	if redirect == "" && resp.NextAction != nil && resp.NextAction.RedirectToURL != nil {
		redirect = strings.TrimSpace(resp.NextAction.RedirectToURL.URL)
	}
	if redirect == "" {
		redirect = strings.TrimSpace(resp.Payment.RedirectURL)
	}
	if redirect != "" {
		return &billing.NextAction{Type: "redirect_to_url", URL: &redirect}
	}
	for _, link := range []string{resp.Payment.SolanaPayURL, resp.Payment.TransactionURL} {
		if link = strings.TrimSpace(link); strings.HasPrefix(link, "solana:") {
			return &billing.NextAction{Type: "solana_pay", URL: &link}
		}
	}
	return nil
}
