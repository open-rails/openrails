package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
)

// Invoices: a customer previews, buys (one call, or create then pay), confirms
// after acting, cancels and reads their own invoices. The invoice id is not a
// credential: every action is the customer's own, and another customer's
// invoice does not exist.

var (
	// ErrIdempotencyKeyInUse: the request first sent with the key is still
	// running.
	ErrIdempotencyKeyInUse = apperr.New(http.StatusConflict, billing.CodeIdempotencyKeyInUse, "The request first sent with this Idempotency-Key is still running.")
	errInvoiceKeyReused    = apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, "The Idempotency-Key was first sent with a different request.")
	errInvoicesFenced      = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "The merchant takes no invoices right now.")
)

// InvoiceActor is the customer acting on their invoices, as the request proved
// them. Principal is the interactive session paying needs. Present is the
// customer in person; Country (a trusted proxy's) and Language
// (Accept-Language) are their request's, recorded on a checkout only then.
type InvoiceActor struct {
	Customer  billing.CustomerID
	Principal billingauth.Payer
	ClientIP  string
	Present   bool
	Country   string
	Language  string
}

// checkout is what the actor's request tells a checkout of its customer.
func (a InvoiceActor) checkout() invoices.CheckoutFacts {
	if !a.Present {
		return invoices.CheckoutFacts{}
	}
	return invoices.CheckoutFacts{IPCountry: a.Country, Language: a.Language}
}

// attribution is what of given a checkout keeps. A field it leaves out is
// counted, never the reason to refuse a sale.
func (s *Service) attribution(ctx context.Context, given *billing.InvoiceAttribution) billing.InvoiceAttribution {
	kept, drops := invoices.CheckAttribution(given)
	for _, drop := range drops {
		mid, _ := merchant.FromContext(ctx)
		opsmetric.Emit(ctx, s.Log, opsmetric.MetricAttributionDropped, []any{"merchant_id", mid.String(), "field", drop.Field, "reason", drop.Reason})
	}
	return kept
}

const (
	invoiceCreateOperation = "invoice_create"
	invoicePayOperation    = "invoice_pay"
)

// invoiceKeyResult is what a key's request answered: the invoice, and the
// answer replayed verbatim (its next action read again, since no client
// secret is stored).
type invoiceKeyResult struct {
	InvoiceID uuid.UUID         `json:"invoice_id"`
	Digest    []byte            `json:"digest"`
	Status    int               `json:"status,omitempty"`
	Code      billing.ErrorCode `json:"code,omitempty"`
	Invoice   *billing.Invoice  `json:"invoice,omitempty"`
}

// InvoiceAnswer is an invoice request's answer: the invoice, and Status 201
// (created), 200, or 402 when its payment failed (Code card_declined or
// payment_failed; payment.last_payment_error says why). Replayed: the
// Idempotency-Key's first request gave this answer.
type InvoiceAnswer struct {
	Invoice  *billing.Invoice
	Status   int
	Code     billing.ErrorCode
	Replayed bool
}

// PreviewInvoice prices lines for the customer without writing, as CreateInvoice
// would buy them: a line held by their own unpaid invoice that CreateInvoice
// replaces is priced as if that invoice were gone.
func (s *Service) PreviewInvoice(ctx context.Context, actor InvoiceActor, params billing.PreviewMyInvoiceParams) (*billing.InvoicePreview, error) {
	return s.previewInvoice(ctx, actor.Customer, &actor, params, !params.Cart)
}

// previewInvoice prices lines for customer; replacing prices them as
// FindOrCreate buys them.
func (s *Service) previewInvoice(ctx context.Context, customer billing.CustomerID, actor *InvoiceActor, params billing.PreviewMyInvoiceParams, replacing bool) (*billing.InvoicePreview, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	lines, err := invoiceLinesOf(ctx, rt, customer, params.Lines, params.Cart)
	if err != nil {
		return nil, err
	}
	preview := rt.Invoices.Preview
	if replacing {
		preview = rt.Invoices.PreviewToReplace
	}
	quote, err := preview(ctx, customer.UUID(), lines)
	if err != nil {
		return nil, err
	}
	// The balance pays an invoice in a merchant's unit: no PSP takes it.
	var options []billing.InvoicePaymentOption
	if !invoices.InMerchantUnit(quote.Currency) {
		if options, err = s.invoiceOptions(ctx, customer.UUID(), actor, quote.Prices(), quote.Products()); err != nil {
			return nil, err
		}
	}
	out := quote.Preview(options)
	return &out, nil
}

// invoicePayment is a request's payment: a saved card, a card just entered or
// a Solana wallet.
type invoicePayment struct {
	method uuid.UUID
	card   *payment.InvoiceCard
	solana *payment.InvoiceSolana
	hosted *payment.InvoiceHosted
}

func readInvoicePayment(p *billing.InvoicePaymentParams, reusable *bool) (invoicePayment, error) {
	if p == nil {
		if reusable != nil {
			return invoicePayment{}, apperr.Invalidf("reusable applies to a new card").WithParam("reusable")
		}
		return invoicePayment{}, nil
	}
	saved, token, wallet, hosted := p.PaymentMethodID != nil && !p.PaymentMethodID.IsZero(), strings.TrimSpace(p.Token) != "", strings.TrimSpace(p.TokenSymbol) != "", strings.TrimSpace(p.ReturnURL) != ""
	switch {
	case btoi(saved)+btoi(token)+btoi(wallet)+btoi(hosted) != 1:
		return invoicePayment{}, apperr.Invalidf("payment names exactly one of payment_method_id, token, token_symbol or return_url").WithParam("payment")
	case saved && (reusable != nil || p.PSPID != nil || p.BillingDetails != nil):
		return invoicePayment{}, apperr.Invalidf("psp_id, billing_details and reusable describe a new card").WithParam("payment")
	case saved:
		return invoicePayment{method: p.PaymentMethodID.UUID()}, nil
	case (wallet || hosted) && (reusable != nil || p.BillingDetails != nil):
		return invoicePayment{}, apperr.Invalidf("billing_details and reusable describe a new card").WithParam("payment")
	case hosted:
		pay := &payment.InvoiceHosted{ReturnURL: strings.TrimSpace(p.ReturnURL)}
		if p.PSPID != nil {
			pay.PSPID = p.PSPID.UUID()
		}
		return invoicePayment{hosted: pay}, nil
	case wallet:
		pay := &payment.InvoiceSolana{TokenSymbol: strings.TrimSpace(p.TokenSymbol)}
		if p.PSPID != nil {
			pay.PSPID = p.PSPID.UUID()
		}
		return invoicePayment{solana: pay}, nil
	}
	card := &payment.InvoiceCard{Token: p.Token, Billing: p.BillingDetails, Reusable: reusable}
	if p.PSPID != nil {
		card.PSPID = p.PSPID.UUID()
	}
	return invoicePayment{card: card}, nil
}

// CreateInvoice answers the customer's open invoice of the lines, or freezes them
// into a new one, and, with a payment, pays it in the same call, or, with a
// pay URL, answers a fresh URL where the customer pays it. key, required
// with a payment, names its attempt: a later request with it answers what
// the first did.
func (s *Service) CreateInvoice(ctx context.Context, actor InvoiceActor, params billing.CreateMyInvoiceParams, key string) (InvoiceAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if params.PayURL == nil {
		return s.createInvoice(ctx, rt, actor, params, key)
	}
	origin, err := s.payURLOrigin(ctx, params)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	params.PayURL = nil // the pay URL is not part of the invoice or its key
	answer, err := s.createInvoice(ctx, rt, actor, params, key)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	return s.attachPayURL(ctx, rt, actor, answer, origin)
}

// createInvoice is CreateInvoice's invoice, under its key.
func (s *Service) createInvoice(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, params billing.CreateMyInvoiceParams, key string) (InvoiceAnswer, error) {
	if err := checkInvoiceLines(params.Lines, params.Cart); err != nil {
		return InvoiceAnswer{}, err
	}
	pay, err := readInvoicePayment(params.Payment, params.Reusable)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if params.Payment != nil && params.ExpectedTotal == nil {
		return InvoiceAnswer{}, apperr.Invalidf("expected_total is required with a payment").WithParam("expected_total")
	}
	if err := s.invoiceCustomerAllowed(ctx, actor.Customer); err != nil {
		return InvoiceAnswer{}, err
	}
	if params.Payment == nil {
		// The customer's open invoice of these lines is answered: asking again,
		// from any tab or reload, makes no second invoice.
		invoice, found, err := s.placeInvoice(ctx, rt, actor, params, key)
		if err != nil {
			return InvoiceAnswer{}, err
		}
		status := http.StatusCreated
		if found {
			status = http.StatusOK
		}
		return s.invoiceAnswer(ctx, rt, actor, invoice.ID, status, payment.InvoiceCharge{}, false, nil)
	}
	// A one-call buy's key names its payment attempt.
	if strings.TrimSpace(key) == "" {
		return InvoiceAnswer{}, apperr.New(http.StatusBadRequest, billing.CodeIdempotencyKeyRequired, "Idempotency-Key header is required with a payment")
	}
	digest, err := s.invoiceCreationDigest(ctx, params, params.Lines)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	customer := actor.Customer.UUID()
	scoped := customer.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, invoiceCreateOperation, scoped)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if claim == nil {
		return s.replayInvoiceAnswer(ctx, rt, actor, rec, digest)
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	id, replayed, charged, err := s.createClaimed(work, rt, actor, params, pay, key)
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return InvoiceAnswer{}, ErrIdempotencyKeyInUse
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	answer, err := s.invoiceAnswer(ctx, rt, actor, id, status, charged, replayed, err)
	s.settleInvoiceClaim(ctx, claim, id, digest, answer, err)
	return answer, err
}

// createClaimed runs a one-call buy under its key's claim: the customer's
// open invoice of the lines (or a new one), paid under the key's attempt.
func (s *Service) createClaimed(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, params billing.CreateMyInvoiceParams, pay invoicePayment, key string) (uuid.UUID, bool, payment.InvoiceCharge, error) {
	customer := actor.Customer.UUID()
	if params.Payment != nil {
		// A payment that cannot be made refuses the invoice before it exists.
		lines, err := invoiceLinesOf(ctx, rt, actor.Customer, params.Lines, params.Cart)
		if err != nil {
			return uuid.Nil, false, payment.InvoiceCharge{}, err
		}
		quote, err := rt.Invoices.Preview(ctx, customer, lines)
		if err == nil {
			err = s.invoicePaymentUsable(ctx, quote, pay, actor.Customer)
		}
		if err != nil {
			return uuid.Nil, false, payment.InvoiceCharge{}, err
		}
	}
	invoice, found, err := s.placeInvoice(ctx, rt, actor, params, "")
	if err != nil {
		return uuid.Nil, false, payment.InvoiceCharge{}, err
	}
	var charged payment.InvoiceCharge
	if invoice.Status == string(billing.InvoiceOpen) {
		charged, err = s.payInvoice(ctx, rt, actor, invoice, pay, "create:"+key)
	}
	return invoice.ID, found, charged, err
}

// placeInvoice answers the customer's open invoice of params' lines (found), or
// creates it. A cart's invoice is always a new one: buying the cart empties it.
// key, the request's Idempotency-Key, names an invoice in a merchant's unit,
// which its balance pays as it is created.
func (s *Service) placeInvoice(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, params billing.CreateMyInvoiceParams, key string) (*invoices.Invoice, bool, error) {
	facts := actor.checkout()
	facts.Attribution = s.attribution(ctx, params.Attribution)
	in := invoices.CreateInput{CustomerID: actor.Customer.UUID(), Origin: billing.InvoiceOriginCustomer, Lines: params.Lines, FromCart: params.Cart,
		ExpectedTotal: params.ExpectedTotal, TTL: invoices.CustomerTTL, Checkout: facts}
	if key = strings.TrimSpace(key); key != "" {
		digest, err := s.invoiceCreationDigest(ctx, params, params.Lines)
		if err != nil {
			return nil, false, err
		}
		in.BalanceKey, in.BalanceDigest = "me:"+actor.Customer.String()+":"+key, digest
	}
	if !params.Cart {
		digest, err := s.invoiceLinesDigest(ctx, params.Lines)
		if err != nil {
			return nil, false, err
		}
		if in.LinesDigest = digest; digest != nil {
			return rt.Invoices.FindOrCreate(ctx, in)
		}
	}
	invoice, err := rt.Invoices.Create(ctx, in)
	return invoice, false, err
}

// invoiceAnswer reads the invoice a request acted on into its answer: 402 when
// its payment failed. err passes through.
func (s *Service) invoiceAnswer(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, id uuid.UUID, status int, charged payment.InvoiceCharge, replayed bool, err error) (InvoiceAnswer, error) {
	if err != nil {
		return InvoiceAnswer{}, err
	}
	view, err := s.invoiceView(ctx, rt, actor, id)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	out := InvoiceAnswer{Invoice: view, Status: status, Replayed: replayed}
	if charged.Failed && view.Payment.Status == billing.InvoicePaymentRequiresPaymentMethod {
		out.Status, out.Code = http.StatusPaymentRequired, billing.CodePaymentFailed
		if charged.Declined {
			out.Code = billing.CodeCardDeclined
		}
	}
	return out, nil
}

// replayInvoiceAnswer answers a key whose request finished with the answer it
// gave.
func (s *Service) replayInvoiceAnswer(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, rec *idempotency.Record, digest []byte) (InvoiceAnswer, error) {
	if rec.Status != idempotency.StatusSucceeded {
		return InvoiceAnswer{}, ErrIdempotencyKeyInUse
	}
	var result invoiceKeyResult
	if err := json.Unmarshal(rec.Result, &result); err != nil {
		return InvoiceAnswer{}, err
	}
	if string(result.Digest) != string(digest) {
		return InvoiceAnswer{}, errInvoiceKeyReused
	}
	if result.Invoice == nil {
		return InvoiceAnswer{}, errors.New("idempotency record holds no answer")
	}
	if result.Invoice.Payment.Status == billing.InvoicePaymentRequiresAction {
		invoice, err := rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), result.InvoiceID)
		if err != nil {
			return InvoiceAnswer{}, err
		}
		if result.Invoice.Payment.NextAction, err = rt.Attempts.InvoiceNextAction(ctx, invoice.AsOf(s.now()), actor.Principal, s.rt.StripeEngines()); err != nil {
			return InvoiceAnswer{}, err
		}
	}
	return InvoiceAnswer{Invoice: result.Invoice, Status: result.Status, Code: result.Code, Replayed: true}, nil
}

// settleInvoiceClaim records a key's answer, its next action left out.
func (s *Service) settleInvoiceClaim(ctx context.Context, claim *idempotency.Claim, invoice uuid.UUID, digest []byte, answer InvoiceAnswer, err error) {
	if err != nil {
		_ = claim.Fail(ctx, err)
		return
	}
	stored := *answer.Invoice
	stored.Payment.NextAction = nil
	raw, _ := json.Marshal(invoiceKeyResult{InvoiceID: invoice, Digest: digest, Status: answer.Status, Code: answer.Code, Invoice: &stored})
	_ = claim.Complete(ctx, raw)
}

// PayInvoice pays the customer's open invoice. One key is one attempt.
func (s *Service) PayInvoice(ctx context.Context, actor InvoiceActor, id billing.InvoiceID, params billing.PayInvoiceParams, key string) (InvoiceAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return InvoiceAnswer{}, err
	}
	pay, err := readInvoicePayment(&params.Payment, params.Reusable)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	invoice, err := rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return InvoiceAnswer{}, invoices.ErrNotPayable
	}
	// A usage invoice's collection is a durable operation that waits out
	// disabled writes and provider recovery itself.
	if invoice.Origin != string(billing.InvoiceOriginUsage) {
		if err := s.invoiceCustomerAllowed(ctx, actor.Customer); err != nil {
			return InvoiceAnswer{}, err
		}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	digest := invoiceDigest(params)
	scoped := invoice.ID.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, invoicePayOperation, scoped)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if claim == nil {
		return s.replayInvoiceAnswer(ctx, rt, actor, rec, digest)
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	var charged payment.InvoiceCharge
	var replayed bool
	switch {
	case params.ExpectedTotal != invoice.Total:
		err = invoices.ErrTotalChanged
	case invoice.Origin == string(billing.InvoiceOriginSubscription):
		charged, replayed, err = s.payRenewal(work, actor, invoice, pay, key)
	case invoice.Origin == string(billing.InvoiceOriginUsage):
		charged, replayed, err = s.payUsage(work, actor, invoice, pay, key)
	default:
		_, attemptErr := rt.DB.Gen(work).GetInvoiceAttempt(work, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: payment.InvoiceAttemptID(mid.UUID(), invoice.ID, "pay:"+key)})
		replayed = attemptErr == nil
		charged, err = s.payInvoice(work, rt, actor, invoice, pay, "pay:"+key)
	}
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return InvoiceAnswer{}, ErrIdempotencyKeyInUse
	}
	answer, err := s.invoiceAnswer(ctx, rt, actor, invoice.ID, http.StatusOK, charged, replayed, err)
	s.settleInvoiceClaim(ctx, claim, invoice.ID, digest, answer, err)
	return answer, err
}

// payInvoice pays a purchase. A saved card awaiting the customer's consent
// (its brand changed) is verified with them present first.
func (s *Service) payInvoice(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, invoice *invoices.Invoice, pay invoicePayment, key string) (payment.InvoiceCharge, error) {
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return payment.InvoiceCharge{}, invoices.ErrNotPayable
	}
	quote, err := s.frozenQuote(ctx, invoice)
	if err != nil {
		return payment.InvoiceCharge{}, err
	}
	if invoice.PurchaseNumber != nil {
		// Asking to pay is recorded before anything may refuse it.
		var country string
		if pay.card != nil && pay.card.Billing != nil && pay.card.Billing.Address != nil && pay.card.Billing.Address.Country != nil {
			country = *pay.card.Billing.Address.Country
		}
		if err := rt.Invoices.MarkSubmitted(ctx, actor.Customer.UUID(), invoice.ID, actor.checkout(), country, pay.method); err != nil {
			s.Log.WarnContext(ctx, "checkout: cannot record its submission", logx.Err(err), "invoice_id", invoice.ID.String())
		}
	}
	if err := s.reconsentPaying(ctx, actor.Customer.UUID(), pay.method, key); err != nil {
		return payment.InvoiceCharge{}, err
	}
	user := &payment.CustomerIdentity{ID: actor.Customer.String(), ClientIP: actor.ClientIP}
	identify := func(ctx context.Context, user *payment.CustomerIdentity) error { return s.identify(ctx, actor, user) }
	return rt.Attempts.PayInvoice(ctx, payment.InvoicePayInput{Invoice: invoice, PaymentMethodID: pay.method, Card: pay.card, Solana: pay.solana, Hosted: pay.hosted, Key: key, User: user,
		Identify: identify, Prices: quote.prices, Products: quote.products})
}

// identify names the customer to a provider's own page (a CCBill FlexForm):
// the verified email and username their credential carries, else the
// merchant's directory's.
func (s *Service) identify(ctx context.Context, actor InvoiceActor, user *payment.CustomerIdentity) error {
	if p := actor.Principal; p.EmailVerified && strings.TrimSpace(p.Email) != "" {
		email := strings.TrimSpace(p.Email)
		user.Email = &email
	}
	user.Username = strings.TrimSpace(actor.Principal.Username)
	if (user.Email != nil && user.Username != "") || s.rt.Contacts == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	found, err := s.rt.Contacts.Contacts(ctx, mid, []uuid.UUID{actor.Customer.UUID()})
	if err != nil {
		return err
	}
	contact, ok := found[actor.Customer.UUID()]
	if !ok {
		return nil
	}
	if email := strings.TrimSpace(contact.Email); user.Email == nil && email != "" {
		user.Email = &email
	}
	if user.Username == "" {
		user.Username = strings.TrimSpace(contact.Username)
	}
	return nil
}

// ConfirmInvoice reads the provider after the customer completed next_action;
// an attempt that failed answers 402.
func (s *Service) ConfirmInvoice(ctx context.Context, actor InvoiceActor, id billing.InvoiceID, params billing.ConfirmInvoiceParams) (InvoiceAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return InvoiceAnswer{}, err
	}
	invoice, err := rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return InvoiceAnswer{}, err
	}
	var signature string
	if params.Signature != nil {
		signature = strings.TrimSpace(*params.Signature)
	}
	charged, err := rt.Attempts.ConfirmInvoice(ctx, invoice, actor.Principal, signature)
	return s.invoiceAnswer(ctx, rt, actor, invoice.ID, http.StatusOK, charged, false, err)
}

// CancelInvoice cancels the customer's open invoice.
func (s *Service) CancelInvoice(ctx context.Context, actor InvoiceActor, id billing.InvoiceID) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	if _, err := rt.Invoices.Cancel(ctx, actor.Customer.UUID(), id.UUID()); err != nil {
		return nil, err
	}
	return s.invoiceView(ctx, rt, actor, id.UUID())
}

// GetInvoice reads the customer's own invoice.
func (s *Service) GetInvoice(ctx context.Context, actor InvoiceActor, id billing.InvoiceID) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	invoice, err := rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return nil, err
	}
	// The payment form loading it is the checkout opened.
	if actor.Present && invoice.PurchaseNumber != nil && invoice.OpenedAt == nil && invoice.Status == string(billing.InvoiceOpen) {
		if err := rt.Invoices.MarkOpened(ctx, actor.Customer.UUID(), invoice.ID, actor.checkout()); err != nil {
			s.Log.WarnContext(ctx, "checkout: cannot record its opening", logx.Err(err), "invoice_id", id.String())
		}
	}
	return s.viewOf(ctx, rt, actor, invoice)
}

// ListInvoices pages the customer's own invoices, newest first. A list carries
// no next_action or payment options; read the invoice for them.
// A page leaves out purchases that expired unpaid: they were never bills.
// Invoices named by id are answered as they are.
func (s *Service) ListInvoices(ctx context.Context, actor InvoiceActor, params billing.InvoiceListParams) (billing.ListPage[billing.Invoice], error) {
	if actor.Customer.IsZero() {
		return billing.ListPage[billing.Invoice]{}, invoices.ErrNotFound
	}
	params.CustomerID = actor.Customer
	return s.listInvoices(ctx, params, false)
}

// ListMerchantInvoices pages the merchant's invoices for staff, newest first.
func (s *Service) ListMerchantInvoices(ctx context.Context, params billing.InvoiceListParams) (billing.ListPage[billing.Invoice], error) {
	return s.listInvoices(ctx, params, true)
}

// GetMerchantInvoice reads one invoice for staff. Staff never pay, so it carries
// no next_action.
func (s *Service) GetMerchantInvoice(ctx context.Context, id billing.InvoiceID) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	invoice, err := rt.Invoices.Get(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	view := invoice.AsOf(s.now()).View(nil, nil)
	view.Checkout = invoice.Checkout()
	return &view, s.markDelinquent(ctx, &view)
}

// listInvoices pages invoices; merchant reads carry their checkout facts.
func (s *Service) listInvoices(ctx context.Context, params billing.InvoiceListParams, merchant bool) (billing.ListPage[billing.Invoice], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Invoice]{}, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return billing.ListPage[billing.Invoice]{}, err
	}
	if params.IDs != nil {
		ids := make([]uuid.UUID, len(params.IDs))
		for i, id := range params.IDs {
			ids[i] = id.UUID()
		}
		rows, err := rt.Invoices.ListByIDs(ctx, ids)
		if err != nil {
			return billing.ListPage[billing.Invoice]{}, err
		}
		out := billing.ListPage[billing.Invoice]{Items: make([]billing.Invoice, 0, len(rows))}
		for _, o := range rows {
			if params.CustomerID.IsZero() || o.CustomerID == params.CustomerID.UUID() {
				view := o.AsOf(s.now()).View(nil, nil)
				if merchant {
					view.Checkout = o.Checkout()
				}
				out.Items = append(out.Items, view)
			}
		}
		return out, s.markDelinquentPage(ctx, out.Items)
	}
	// A customer's page leaves out its purchases that expired unpaid.
	filter := invoices.ListFilter{OmitExpiredPurchases: !merchant}
	if !params.CustomerID.IsZero() {
		filter.CustomerID = new(params.CustomerID.UUID())
	}
	if !params.PriceID.IsZero() {
		price, err := s.rt.PriceService.GetByPublicID(ctx, params.PriceID)
		if db.IsNotFound(err) {
			return billing.ListPage[billing.Invoice]{Items: []billing.Invoice{}}, nil
		}
		if err != nil {
			return billing.ListPage[billing.Invoice]{}, err
		}
		filter.PriceID = &price.ID
	}
	if params.Status != "" {
		filter.Status = new(string(params.Status))
	}
	if params.Origin != "" {
		filter.Origin = new(string(params.Origin))
	}
	if !params.SubscriptionID.IsZero() {
		filter.SubscriptionID = new(params.SubscriptionID.UUID())
	}
	if params.Overdue {
		filter.OverdueAt = new(s.now())
	}
	page, err := rt.Invoices.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Invoice]{}, err
	}
	out := billing.ListPage[billing.Invoice]{Items: make([]billing.Invoice, len(page.Items)), Next: page.Next}
	for i, o := range page.Items {
		out.Items[i] = o.AsOf(s.now()).View(nil, nil)
		if merchant {
			out.Items[i].Checkout = o.Checkout()
		}
	}
	return out, s.markDelinquentPage(ctx, out.Items)
}

// invoiceView is the invoice as its customer reads it, with the next action of a
// payment awaiting them and the options of an open invoice.
func (s *Service) invoiceView(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, id uuid.UUID) (*billing.Invoice, error) {
	invoice, err := rt.Invoices.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, rt, actor, invoice)
}

// viewOf is invoiceView of a loaded invoice.
func (s *Service) viewOf(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, invoice *invoices.Invoice) (*billing.Invoice, error) {
	var err error
	invoice.AsOf(s.now())
	var next *billing.NextAction
	if invoice.CollectionOwner == string(billing.InvoiceCollectionEngine) && invoice.AwaitsCustomer() {
		if next, err = rt.Attempts.InvoiceNextAction(ctx, invoice, actor.Principal, s.rt.StripeEngines()); err != nil {
			return nil, err
		}
	}
	options, err := s.openInvoiceOptions(ctx, invoice, &actor)
	if err != nil {
		return nil, err
	}
	view := invoice.View(next, options)
	return &view, s.markDelinquent(ctx, &view)
}

// openInvoiceOptions are what can pay an open invoice, each with the invoice's
// failed attempts through it; none for any other invoice.
func (s *Service) openInvoiceOptions(ctx context.Context, invoice *invoices.Invoice, actor *InvoiceActor) ([]billing.InvoicePaymentOption, error) {
	var options []billing.InvoicePaymentOption
	var err error
	switch {
	case invoice.CollectionOwner != string(billing.InvoiceCollectionEngine):
		return nil, nil
	case invoice.Status != string(billing.InvoiceOpen):
		return nil, nil
	case invoice.Origin == string(billing.InvoiceOriginSubscription):
		options, err = s.renewalOptions(ctx, invoice)
	case invoice.Origin == string(billing.InvoiceOriginUsage):
		options, err = s.usageOptions(ctx, invoice)
	default:
		var quote frozenLines
		if quote, err = s.frozenQuote(ctx, invoice); err == nil {
			options, err = s.invoiceOptions(ctx, invoice.CustomerID, actor, quote.prices, quote.products)
		}
	}
	if err != nil || len(options) == 0 {
		return options, err
	}
	failed, err := s.rt.InvoiceAttemptService.FailedAttempts(ctx, invoice.ID)
	if err != nil {
		return nil, err
	}
	for i := range options {
		options[i].FailedAttempts = failed[options[i].PSPID.UUID()]
	}
	return options, nil
}

type frozenLines struct {
	prices   []*models.Price
	products []*models.Product
}

// frozenQuote reads the catalog rows an invoice's lines name, for routing.
func (s *Service) frozenQuote(ctx context.Context, invoice *invoices.Invoice) (frozenLines, error) {
	var out frozenLines
	for _, l := range invoice.Lines {
		if l.PriceID == nil || l.ProductID == nil {
			return out, invoices.ErrNotPayable
		}
		price, err := s.rt.PriceService.GetByID(ctx, *l.PriceID)
		if err != nil {
			return out, err
		}
		product, err := s.rt.ProductService.GetByID(ctx, *l.ProductID)
		if err != nil {
			return out, err
		}
		out.prices, out.products = append(out.prices, price), append(out.products, product)
	}
	return out, nil
}

// invoiceOptions are the PSPs that sell the lines. For customer, the customer
// reading them, a CCBill option names what its page still needs (identify).
func (s *Service) invoiceOptions(ctx context.Context, customer uuid.UUID, actor *InvoiceActor, prices []*models.Price, products []*models.Product) ([]billing.InvoicePaymentOption, error) {
	if page, ok := paymentpage.FromContext(ctx); ok && !page.SavedPaymentMethods {
		customer = uuid.Nil
	}
	options, err := s.rt.InvoiceAttemptService.CustomerInvoiceOptions(ctx, customer, prices, products)
	if err != nil {
		return nil, err
	}
	out := make([]billing.InvoicePaymentOption, 0, len(options))
	for _, o := range options {
		var saved []billing.PaymentMethodID
		for _, id := range o.SavedPaymentMethodIDs {
			saved = append(saved, billing.PaymentMethodID(id))
		}
		option := billing.InvoicePaymentOption{PSPID: billing.PSPID(o.PSPID), Rail: billing.Rail(o.Rail), Accepts: o.Accepts(), SavedPaymentMethodIDs: saved}
		if actor != nil && o.Rail == string(models.RailCCBill) {
			// A hint only: paying checks again, so a directory outage
			// leaves the option without it rather than failing the read.
			user := &payment.CustomerIdentity{ID: actor.Customer.String()}
			if err := s.identify(ctx, *actor, user); err != nil {
				s.Log.WarnContext(ctx, "ccbill option: cannot read the customer's identity", logx.Err(err), "customer_id", actor.Customer.String())
			} else {
				option.Requires = payment.CCBillIdentityMissing(user)
			}
		}
		out = append(out, option)
	}
	return out, nil
}

// invoicePaymentUsable refuses, before an invoice exists, a saved card no PSP
// that sells its lines can charge, or a new card's PSP that takes none.
func (s *Service) invoicePaymentUsable(ctx context.Context, quote *invoices.Quote, pay invoicePayment, customer billing.CustomerID) error {
	if invoices.InMerchantUnit(quote.Currency) {
		return apperr.Invalidf("an invoice in a merchant's unit is paid from the customer's balance: send no payment").WithParam("payment")
	}
	for i := range quote.Lines {
		l := &quote.Lines[i]
		if l.Refusal == nil {
			continue
		}
		refused := &invoices.LineError{Index: i, Refusal: *l.Refusal}
		if _, held := refused.HeldBy(); !held {
			return refused
		}
		// The customer's own unpaid invoice holding the line is FindOrCreate's
		// to answer or replace; the payment is checked for the line itself.
		l.Refusal = nil
	}
	options, err := s.rt.InvoiceAttemptService.CustomerInvoiceOptions(ctx, customer.UUID(), quote.Prices(), quote.Products())
	if err != nil {
		return err
	}
	if pay.card != nil {
		option, err := payment.NewCardOption(options, pay.card.PSPID)
		if err != nil {
			return err
		}
		_, err = s.rt.InvoiceAttemptService.NewCardBilling(ctx, *option, pay.card.Billing)
		return err
	}
	if pay.solana != nil {
		_, err := payment.SolanaOption(options, pay.solana.PSPID)
		return err
	}
	if pay.hosted != nil {
		if err := s.rt.InvoiceAttemptService.CheckReturnURL(ctx, pay.hosted.ReturnURL); err != nil {
			return err
		}
		_, err := payment.HostedOption(options, pay.hosted.PSPID)
		return err
	}
	pm, err := s.rt.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pay.method, customer.String())
	if err != nil {
		return fmt.Errorf("%w: %w", payment.ErrPaymentMethodStale, err)
	}
	if pm.Status != paymentmethods.StatusActive {
		return fmt.Errorf("%w: the card is %s", payment.ErrPaymentMethodStale, pm.Status)
	}
	for _, o := range options {
		if pm.Custodian == models.CustodianPlatform {
			if slices.Contains(o.SavedPaymentMethodIDs, pay.method) {
				return nil
			}
			continue
		}
		if strings.EqualFold(string(pm.Rail), o.Rail) && pm.ChargeableOn(o.PSPID) {
			return nil
		}
	}
	return invoices.ErrOptionMissing
}

// invoiceCustomerAllowed refuses every customer of a merchant whose provider
// writes are fenced.
func (s *Service) invoiceCustomerAllowed(ctx context.Context, _ billing.CustomerID) error {
	if err := s.rt.InvoiceAttemptService.TakesInvoices(ctx); err != nil {
		if errors.Is(err, payment.ErrProviderWritesDisabled) {
			return err
		}
		return errInvoicesFenced
	}
	return nil
}

type invoiceRuntime struct {
	Invoices    *invoices.Service
	Attempts    *payment.InvoiceAttemptService
	Idempotency *idempotency.Store
	DB          interface {
		Gen(context.Context) *gen.Queries
	}
}

func (s *Service) invoiceRuntime() (*invoiceRuntime, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Invoices == nil || rt.InvoiceAttemptService == nil || rt.Idempotency == nil || rt.DB == nil || rt.PriceService == nil || rt.ProductService == nil || rt.PaymentMethodService == nil {
		return nil, errors.New("billing service: invoices unavailable")
	}
	return &invoiceRuntime{Invoices: rt.Invoices, Attempts: rt.InvoiceAttemptService, Idempotency: rt.Idempotency, DB: rt.DB}, nil
}

func invoiceDigest(v any) []byte {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return sum[:]
}

// RecordPayment records money the merchant received outside OpenRails for an
// invoice: part or all of what a usage invoice owes, or another invoice's total.
// created is false when the same remittance was already recorded.
func (s *Service) RecordPayment(ctx context.Context, params billing.RecordExternalPaymentParams) (payment *models.Payment, created bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, false, err
	}
	invoice, err := rt.Invoices.Get(ctx, params.InvoiceID.UUID())
	if err != nil {
		return nil, false, err
	}
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return nil, false, invoices.ErrNotPayable
	}
	if invoice.Origin == string(billing.InvoiceOriginUsage) {
		payment, created, err = s.moneyService().RecordUsageRemittance(ctx, money.UsageRemittance{InvoiceID: invoice.ID, Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
		if errors.Is(err, money.ErrUsageActionNotAllowed) {
			err = errUsageActionRefused
		}
		return payment, created, err
	}
	return rt.Invoices.RecordRemittance(ctx, invoices.Remittance{InvoiceID: params.InvoiceID.UUID(), Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
}

// Merchant invoices: the merchant's backend prices lines for a customer and
// creates an unpaid invoice they pay on the merchant's own site. Merchants
// never pay: no merchant route takes a payment.

// PreviewMerchantInvoice prices lines for one customer without writing.
func (s *Service) PreviewMerchantInvoice(ctx context.Context, params billing.PreviewInvoiceParams) (*billing.InvoicePreview, error) {
	if params.CustomerID.IsZero() {
		return nil, apperr.Invalidf("customer_id required").WithParam("customer_id")
	}
	return s.previewInvoice(ctx, params.CustomerID, nil, billing.PreviewMyInvoiceParams{Lines: params.Lines}, false)
}

// CreateMerchantInvoice creates an unpaid invoice for a customer. key is the
// request's Idempotency-Key, kept on the invoice and unique per merchant: the
// same key answers the same invoice (replayed), another body with it (another
// customer's included) is idempotency_key_reused.
func (s *Service) CreateMerchantInvoice(ctx context.Context, params billing.CreateInvoiceParams, key string) (*billing.Invoice, bool, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, false, err
	}
	if params.CustomerID.IsZero() {
		return nil, false, apperr.Invalidf("customer_id required").WithParam("customer_id")
	}
	if err := invoices.CheckLines(params.Lines); err != nil {
		return nil, false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, apperr.New(http.StatusBadRequest, billing.CodeIdempotencyKeyRequired, "Idempotency-Key header is required")
	}
	if err := s.invoiceCustomerAllowed(ctx, params.CustomerID); err != nil {
		return nil, false, err
	}
	digest, err := s.invoiceCreationDigest(ctx, params, params.Lines)
	if err != nil {
		return nil, false, err
	}
	scoped := "app:" + key
	existing, err := rt.Invoices.ByIdempotencyKey(ctx, scoped)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if string(existing.RequestDigest) != string(digest) {
			return nil, false, errInvoiceKeyReused
		}
		existing, err = rt.Invoices.ExpireIfDue(ctx, existing)
		if err != nil {
			return nil, true, err
		}
		view, err := s.merchantInvoiceView(ctx, existing)
		if err != nil {
			return nil, true, err
		}
		if view.PayURLExpiresAt != nil {
			recovered, err := paymentpage.ReplayURL(ctx, rt.DB.Gen(ctx), s.rt.CheckoutOrigin(), existing.BillingInvoice, s.now())
			if err != nil {
				return nil, true, err
			}
			if recovered != "" {
				view.PayURL = &recovered
			}
		}
		return view, true, nil
	}
	now := s.now()
	// A pay URL is checked before the invoice exists; it expires on its own,
	// and the invoice stays open after it.
	var attach func(context.Context, *gen.Queries, uuid.UUID) error
	var payURL string
	if params.PayURL != nil {
		origin := s.rt.CheckoutOrigin()
		if origin == "" {
			return nil, false, apperr.New(http.StatusBadRequest, billing.CodePayURLUnavailable, "this deployment serves no checkout origin").WithParam("pay_url")
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, false, err
		}
		allowed, err := s.rt.ReturnOrigins(ctx, mid)
		if err != nil {
			return nil, false, err
		}
		plan, err := paymentpage.Plan(*params.PayURL, billing.InvoiceID{}, now, allowed)
		if err != nil {
			return nil, false, err
		}
		if plan.Embedded {
			if _, err := s.embedOrigins(ctx); err != nil {
				if errors.Is(err, errEmbedOriginsRequired) {
					err = errEmbedOriginsRequired.WithParam("pay_url.embedded")
				}
				return nil, false, err
			}
		}
		attach = func(ctx context.Context, q *gen.Queries, id uuid.UUID) error {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			c, err := paymentpage.Plan(*params.PayURL, billing.InvoiceID(id), now, allowed)
			if err != nil {
				return err
			}
			payURL, err = paymentpage.Attach(ctx, q, origin, mid, c, now)
			return err
		}
	}
	invoice, err := rt.Invoices.Create(ctx, invoices.CreateInput{CustomerID: params.CustomerID.UUID(), Origin: billing.InvoiceOriginMerchant, Lines: params.Lines,
		IdempotencyKey: scoped, RequestDigest: digest, Attach: attach, Checkout: invoices.CheckoutFacts{Attribution: s.attribution(ctx, params.Attribution)}})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, false, ErrIdempotencyKeyInUse
		}
		return nil, false, err
	}
	if params.PayURL != nil {
		// A Stripe PSP declared before the checkout origin was served is
		// registered as its payment-method domain now.
		if mid, err := merchant.Require(ctx); err == nil {
			s.rt.Go("payment domain registration", func(ctx context.Context) {
				if err := s.rt.RegisterPaymentDomain(ctx, mid); err != nil && ctx.Err() == nil {
					s.Log.WarnContext(ctx, "payment domain: cannot queue its registration", logx.Err(err), "merchant_id", mid.String())
				}
			})
		}
	}
	view, err := s.merchantInvoiceView(ctx, invoice)
	if err == nil && payURL != "" && view.PayURLExpiresAt != nil {
		// Only creation and its authenticated retry return the capability.
		view.PayURL = &payURL
	}
	return view, false, err
}

// embedOrigins are the origins the merchant registered to frame its
// embedded pay URLs: embed_origins_required without one.
func (s *Service) embedOrigins(ctx context.Context) ([]string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.rt.Merchants == nil {
		return nil, errEmbedOriginsRequired
	}
	_, settings, err := s.rt.Merchants.MerchantSettings(ctx, mid)
	if err != nil {
		return nil, err
	}
	origins := merchantconfig.EmbedOrigins(settings)
	if len(origins) == 0 {
		return nil, errEmbedOriginsRequired
	}
	return origins, nil
}

var errEmbedOriginsRequired = apperr.New(http.StatusBadRequest, billing.CodeEmbedOriginsRequired,
	"an embedded pay URL needs the origins that may frame it: the merchant's settings checkout.embed_origins")

// payURLOrigin is the checkout origin a customer's invoice's pay URL is on,
// checked before the invoice exists: the merchant registered the origins that
// frame it. The request names no URL.
func (s *Service) payURLOrigin(ctx context.Context, params billing.CreateMyInvoiceParams) (string, error) {
	switch {
	case params.Payment != nil:
		return "", apperr.Invalidf("pay_url and payment are exclusive: the pay URL takes the payment").WithParam("pay_url")
	case !params.PayURL.Embedded:
		return "", apperr.Invalidf("pay_url.embedded is true: a customer's pay URL is framed by the merchant's registered origins").WithParam("pay_url.embedded")
	}
	origin := s.rt.CheckoutOrigin()
	if origin == "" {
		return "", apperr.New(http.StatusBadRequest, billing.CodePayURLUnavailable, "this deployment serves no checkout origin").WithParam("pay_url")
	}
	if _, err := s.embedOrigins(ctx); err != nil {
		return "", err
	}
	return origin, nil
}

// attachPayURL gives the answer's invoice, read as it is now (a retry's
// answer is the first's), its embedded pay URL in place of the one it had:
// a fresh URL on every answer. A paid or closed invoice takes none.
func (s *Service) attachPayURL(ctx context.Context, rt *invoiceRuntime, actor InvoiceActor, answer InvoiceAnswer, origin string) (InvoiceAnswer, error) {
	live, err := s.invoiceView(ctx, rt, actor, answer.Invoice.ID.UUID())
	if err != nil {
		return InvoiceAnswer{}, err
	}
	invoice := *live
	answer.Invoice = &invoice
	if invoice.Status != billing.InvoiceOpen || invoice.CollectionOwner != billing.InvoiceCollectionEngine {
		return answer, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return InvoiceAnswer{}, err
	}
	c, url, ok, err := paymentpage.AttachMine(ctx, rt.DB.Gen(ctx), origin, mid, actor.Customer, invoice.ID, invoice.ExpiresAt, s.now())
	switch {
	case err != nil:
		return InvoiceAnswer{}, err
	case !ok:
		return answer, nil
	}
	s.rt.Go("payment domain registration", func(ctx context.Context) {
		if err := s.rt.RegisterPaymentDomain(ctx, mid); err != nil && ctx.Err() == nil {
			s.Log.WarnContext(ctx, "payment domain: cannot queue its registration", logx.Err(err), "merchant_id", mid.String())
		}
	})
	c.Show(&invoice, &url)
	return answer, nil
}

// CancelMerchantInvoice withdraws an open invoice the merchant created.
func (s *Service) CancelMerchantInvoice(ctx context.Context, id billing.InvoiceID) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	invoice, err := rt.Invoices.CancelMerchantInvoice(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	return s.merchantInvoiceView(ctx, invoice)
}

// merchantInvoiceView is an invoice as the merchant reads it: what can pay it,
// never a next action (only its customer pays).
func (s *Service) merchantInvoiceView(ctx context.Context, invoice *invoices.Invoice) (*billing.Invoice, error) {
	options, err := s.openInvoiceOptions(ctx, invoice.AsOf(s.now()), nil)
	if err != nil {
		return nil, err
	}
	view := invoice.View(nil, options)
	view.Checkout = invoice.Checkout()
	return &view, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
