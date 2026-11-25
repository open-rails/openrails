package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/cardholdername"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Invoices are paid by invoice attempts (mode invoice) whose charge is the sale
// intent: the same admission, submission fence, receipt and verification as a
// one-price sale, settling the invoice's own lines.

// invoiceAttemptTTL bounds one attempt; the invoice's own expiry bounds it too.
const invoiceAttemptTTL = 24 * time.Hour

// SetInvoices arms invoice payment.
func (s *InvoiceAttemptService) SetInvoices(o *invoices.Service) {
	if s == nil {
		return
	}
	s.invoices = o
	if o != nil {
		o.Abandon = s.AbandonInvoiceAttempt
	}
	if sale := s.saleService(); sale != nil {
		sale.Invoices = o
	}
}

func (s *InvoiceAttemptService) saleService() *NMISaleService {
	cs := s.purchaseService
	if cs == nil {
		return nil
	}
	return cs.NMISaleService
}

// InvoiceOption is one PSP that can take every line of an invoice. Hosted: the
// invoice can be paid on the PSP's own page (Stripe Checkout for one-time
// lines, a CCBill FlexForm for one subscription). Custodian is the kind of
// the third party holding the cards the PSP charges, CustodianID its
// account; "" when the PSP holds its own.
type InvoiceOption struct {
	PSPID                 uuid.UUID
	Rail                  string
	Hosted                bool
	Custodian             string
	CustodianID           *uuid.UUID
	SavedPaymentMethodIDs []uuid.UUID
	// PriceID is the product's CCBill provider price whose FlexForm sells an
	// invoice's one line of ours on the same terms; nil for the line's own.
	PriceID uuid.UUID
}

// Solana reports a Solana Pay option.
func (o InvoiceOption) Solana() bool { return models.Rail(o.Rail) == models.RailSolana }

// Accepts is how the option is paid: saved_card, new_card and on a hosted
// page redirect; or solana_pay. A Basis Theory PSP takes the card its
// Elements collected.
func (o InvoiceOption) Accepts() []string {
	switch {
	case o.Solana():
		return []string{"solana_pay"}
	case models.Rail(o.Rail) == models.RailCCBill:
		return []string{"redirect"}
	case o.Custodian == models.CustodianBasisTheory || o.Custodian == models.CustodianPlatform:
		if len(o.SavedPaymentMethodIDs) > 0 {
			return []string{"new_card", "saved_card"}
		}
		return []string{"new_card"}
	case o.Hosted:
		return []string{"saved_card", "new_card", "redirect"}
	}
	return []string{"saved_card", "new_card"}
}

// InvoiceOptions are the PSPs that sell every line: a card on NMI or Stripe,
// every recurring line an engine-owned subscription; a Solana wallet, for
// one-time lines or the one line of a price with a Solana plan; or a CCBill
// FlexForm, for the one line of a recurring price. A Basis Theory
// custodian's PSP sells one-time lines.
func (s *InvoiceAttemptService) InvoiceOptions(ctx context.Context, prices []*models.Price, products []*models.Product) ([]InvoiceOption, error) {
	if len(prices) == 0 || len(prices) != len(products) {
		return []InvoiceOption{}, nil
	}
	// Routing reads each price's PSP links, which a quote's rows lack.
	linked := make([]*models.Price, len(prices))
	for i, price := range prices {
		copied := *price
		linked[i] = &copied
	}
	if err := s.db.LoadPricePSPBindings(ctx, linked, nil); err != nil {
		return nil, err
	}
	var out []InvoiceOption
	for i, price := range linked {
		decision, err := s.Route(ctx, RoutingInput{Price: price, Product: products[i], Mode: purchaseModeForRail(price, "")})
		if err != nil && !errors.Is(err, ErrNoRoutableProcessor) {
			return nil, err
		}
		var line []InvoiceOption
		for _, c := range decision.Candidates {
			rail := models.Rail(c.Rail)
			if products[i].CreditGrant != nil && price.IsRecurring() && rail != models.RailStripe && rail != models.RailNMI {
				continue // recurring credit is delivered by the engine's renewal invoice
			}
			if c.Skip != "" || c.PSPID == uuid.Nil || (rail != models.RailNMI && rail != models.RailStripe && rail != models.RailSolana && rail != models.RailCCBill) {
				continue
			}
			// A Solana plan subscribes an invoice of its one line; so does a
			// CCBill FlexForm, which sells nothing else.
			plan := rail == models.RailSolana && len(linked) == 1 && priceHasSolanaRecurring(price.ForPSP(c.PSPID))
			form := rail == models.RailCCBill && len(linked) == 1 && price.IsRecurring()
			if rail == models.RailCCBill && !form {
				continue
			}
			if price.IsRecurring() && !plan && !form && (rail == models.RailSolana || rails.NewSubscriptionFor(rail) != rails.NewSubscriptionEngine) {
				continue
			}
			if i == 0 || containsOption(out, c.PSPID) {
				line = append(line, InvoiceOption{PSPID: c.PSPID, Rail: c.Rail})
			}
		}
		out = line
	}
	if len(linked) == 1 && products[0].CreditGrant == nil {
		stand, err := s.ccbillStandIns(ctx, linked[0], products[0])
		if err != nil {
			return nil, err
		}
		out = append(out, stand...)
	}
	oneTime := true
	for _, price := range prices {
		oneTime = oneTime && !price.IsRecurring()
	}
	options := []InvoiceOption{}
	for _, o := range out {
		o.Hosted = oneTime && models.Rail(o.Rail) == models.RailStripe || models.Rail(o.Rail) == models.RailCCBill
		if models.Rail(o.Rail) == models.RailNMI {
			var err error
			if o.Custodian, o.CustodianID, err = s.custodyOf(ctx, o); err != nil {
				return nil, err
			}
		}
		options = append(options, o)
	}
	return options, nil
}

// ccbillStandIns are the CCBill options for an invoice's one recurring line of
// ours: the product's live CCBill provider prices on the same terms, each sold
// on its own FlexForm. Ours never sell through CCBill; its prices do.
func (s *InvoiceAttemptService) ccbillStandIns(ctx context.Context, price *models.Price, product *models.Product) ([]InvoiceOption, error) {
	if price.ProviderOwned || !price.SellsVia(catalog.SellViaCard) || !price.IsRecurring() || s.priceService == nil || product == nil {
		return nil, nil
	}
	candidates, err := s.priceService.GetActiveByProductID(ctx, product.ID)
	if err != nil {
		return nil, err
	}
	var out []InvoiceOption
	for _, candidate := range candidates {
		_, link, ok := candidate.ProviderLink()
		if !ok || link[models.RailKeyRail] != string(models.RailCCBill) || !sameSaleTerms(price, candidate) {
			continue
		}
		decision, err := s.Route(ctx, RoutingInput{Price: candidate, Product: product, Mode: models.PurchaseSubscription})
		if err != nil && !errors.Is(err, ErrNoRoutableProcessor) {
			return nil, err
		}
		for _, c := range decision.Candidates {
			if c.Skip == "" && c.PSPID != uuid.Nil && models.Rail(c.Rail) == models.RailCCBill && !containsOption(out, c.PSPID) {
				out = append(out, InvoiceOption{PSPID: c.PSPID, Rail: c.Rail, PriceID: candidate.ID})
			}
		}
	}
	return out, nil
}

// sameSaleTerms reports two prices a customer pays and is granted alike.
func sameSaleTerms(a, b *models.Price) bool {
	eq := func(x, y *int) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.Amount == b.Amount && a.Currency == b.Currency && eq(a.BillingIntervalHours, b.BillingIntervalHours) && eq(a.BillingIntervalMonths, b.BillingIntervalMonths) && eq(a.AccessDurationHours, saleAccessHours(b)) &&
		a.TrialUnitAmount == nil && b.TrialUnitAmount == nil && a.Quantity == nil && b.Quantity == nil
}

// A CCBill default is its declared fixed service interval. Calendar providers
// never stand in for our prices; only equal accepted card-sale terms can.
func saleAccessHours(p *models.Price) *int {
	if p.ProviderAccessDefault {
		return p.BillingIntervalHours
	}
	return p.AccessDurationHours
}

// CustomerInvoiceOptions adds saved platform cards with active reuse consent on
// an offered NMI account. Changing custody for new cards does not move these cards.
func (s *InvoiceAttemptService) CustomerInvoiceOptions(ctx context.Context, customer uuid.UUID, prices []*models.Price, products []*models.Product) ([]InvoiceOption, error) {
	options, err := s.InvoiceOptions(ctx, prices, products)
	if err != nil || customer == uuid.Nil {
		return options, err
	}
	if !slices.ContainsFunc(options, func(o InvoiceOption) bool { return o.Rail == string(models.RailNMI) }) {
		return options, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.db.Gen(ctx)
	methods, err := q.ListPaymentMethodsByCustomer(ctx, gen.ListPaymentMethodsByCustomerParams{MerchantID: mid.UUID(), CustomerID: customer})
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, method := range methods {
		if models.BasisTheoryCustody(method.Custodian) && method.Rail == string(models.RailNMI) && paymentmethods.Chargeable(method) && method.CustodianID != nil && method.RailMethodRef != nil && *method.RailMethodRef != "" {
			ids = append(ids, method.ID)
		}
	}
	if len(ids) == 0 {
		return options, nil
	}
	agreements, err := q.ListLiveMandatesOfPaymentMethods(ctx, gen.ListLiveMandatesOfPaymentMethodsParams{MerchantID: mid.UUID(), CustomerID: customer, PaymentMethodIds: ids})
	if err != nil {
		return nil, err
	}
	for _, agreement := range agreements {
		if agreement.Kind != string(charge.AgreementCardOnFile) || agreement.Status != mandates.StatusActive || agreement.InitialTransactionID == nil || *agreement.InitialTransactionID == "" {
			continue
		}
		for i := range options {
			if options[i].Rail == string(models.RailNMI) && options[i].PSPID == agreement.PspID {
				options[i].SavedPaymentMethodIDs = append(options[i].SavedPaymentMethodIDs, *agreement.PaymentMethodID)
			}
		}
	}
	return options, nil
}

// FailedAttempts counts an invoice's failed attempts by PSP.
func (s *InvoiceAttemptService) FailedAttempts(ctx context.Context, invoice uuid.UUID) (map[uuid.UUID]int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).CountInvoiceFailedAttemptsByPSP(ctx, gen.CountInvoiceFailedAttemptsByPSPParams{MerchantID: mid.UUID(), InvoiceID: invoice})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		out[row.PspID] = int(row.Failed)
	}
	return out, nil
}

// custodyOf is who holds the cards an NMI option's PSP charges.
func (s *InvoiceAttemptService) custodyOf(ctx context.Context, o InvoiceOption) (string, *uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", nil, err
	}
	if svc := merchants.Of(s.db); svc != nil {
		platform, err := svc.PlatformCardCustodian(ctx, mid)
		if err != nil {
			return "", nil, err
		}
		if platform != nil {
			return models.CustodianPlatform, &platform.ID, nil
		}
	}

	purchase := s.purchaseService
	if purchase == nil {
		return "", nil, errors.New("invoice payment is unavailable")
	}
	target, err := purchase.resolveRailTargetForPSP(ctx, o.Rail, o.PSPID)
	if err != nil {
		return "", nil, err
	}
	if target.Scope == nil || target.Scope.CustodianID == nil {
		return "", nil, nil
	}
	custodian, err := s.db.Gen(ctx).GetCustodian(ctx, gen.GetCustodianParams{MerchantID: mid.UUID(), ID: *target.Scope.CustodianID})
	if err != nil {
		return "", nil, err
	}
	return custodian.Kind, target.Scope.CustodianID, nil
}

func containsOption(options []InvoiceOption, psp uuid.UUID) bool {
	for _, o := range options {
		if o.PSPID == psp {
			return true
		}
	}
	return false
}

// InvoicePayInput pays an invoice with a saved card (PaymentMethodID), a card
// just entered (Card) or a Solana wallet (Solana). Key is the request's
// scoped Idempotency-Key: one key is one attempt, forever.
type InvoicePayInput struct {
	Invoice         *invoices.Invoice
	PaymentMethodID uuid.UUID
	Card            *InvoiceCard
	Solana          *InvoiceSolana
	Hosted          *InvoiceHosted
	Key             string
	User            *UserIdentity
	// Identify fills User's verified email and username, which a provider's
	// own page (a CCBill FlexForm) names the customer by.
	Identify func(context.Context, *UserIdentity) error
	Prices   []*models.Price
	Products []*models.Product
}

// InvoiceCard is a card just entered in a PSP's own fields: its single-use
// token, the PSP whose fields made it (zero: the one option that takes new
// cards), its billing details and the customer's reuse choice.
type InvoiceCard struct {
	Token    string
	PSPID    uuid.UUID
	Billing  *billing.BillingDetails
	Reusable *bool
}

// InvoiceCharge is what paying did: Failed when its attempt ended without
// moving money, Declined when the issuer declined it.
type InvoiceCharge struct{ Failed, Declined bool }

// ErrInvoicePSPRequired: more than one PSP takes new cards for the invoice.
var ErrInvoicePSPRequired = apperr.Invalidf("more than one PSP takes this payment for the invoice: name payment.psp_id").WithParam("payment.psp_id")

// TakesInvoices refuses while the merchant's provider writes are fenced or
// held: such a merchant takes no invoices.
func (s *InvoiceAttemptService) TakesInvoices(ctx context.Context) error {
	return s.requireProviderWrites(ctx)
}

// InvoiceAttemptID is the attempt a key makes on an invoice.
func InvoiceAttemptID(merchantID, invoiceID uuid.UUID, key string) uuid.UUID {
	return uuidutil.DerivedV7(uuidutil.TimeOf(invoiceID), uuid.NameSpaceURL, "openrails:order-attempt:"+merchantID.String()+":"+invoiceID.String()+":"+key)
}

// PayInvoice starts (or, for the same key, resumes) one attempt on the invoice
// and runs its charge inline. The invoice reflects the outcome: complete, open
// with last_payment_error, awaiting the customer's action, or processing. A
// new card is charged and saved in one provider request.
func (s *InvoiceAttemptService) PayInvoice(ctx context.Context, in InvoicePayInput) (InvoiceCharge, error) {
	payments := 0
	for _, named := range []bool{in.PaymentMethodID != uuid.Nil, in.Card != nil, in.Solana != nil, in.Hosted != nil} {
		if named {
			payments++
		}
	}
	if in.Invoice == nil || in.User == nil || in.Key == "" || payments != 1 {
		return InvoiceCharge{}, errors.New("invoice payment needs its invoice, customer, key and one payment")
	}
	if in.Invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return InvoiceCharge{}, invoices.ErrNotPayable
	}
	if err := s.requireEngineInvoice(ctx, in.Invoice); err != nil {
		return InvoiceCharge{}, err
	}
	switch {
	case in.Solana != nil:
		return s.payInvoiceSolana(ctx, in)
	case in.Hosted != nil:
		return s.payInvoiceHosted(ctx, in)
	}
	if err := s.GuardCardAttempt(ctx, in.User); err != nil {
		return InvoiceCharge{}, err
	}
	out, err := s.payInvoice(ctx, in)
	if out.Declined || err != nil && cardAttemptFailed(err) {
		s.NoteCardAttempt(ctx, in.User)
	}
	return out, err
}

// requireEngineInvoice rechecks a cached read before entering a generic pay or
// confirm path, including an existing intent's replay. New attempt admission
// checks again in the transaction that binds that attempt.
func (s *InvoiceAttemptService) requireEngineInvoice(ctx context.Context, invoice *invoices.Invoice) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		current, err := s.db.NewWithPgxTx(tx).Gen(ctx).LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: invoice.ID})
		if err != nil {
			return err
		}
		if current.CustomerID != invoice.CustomerID || current.CollectionOwner != string(billing.InvoiceCollectionEngine) {
			return invoices.ErrNotPayable
		}
		return nil
	})
}

// invoiceComplete reports whether the invoice is complete now.
func (s *InvoiceAttemptService) invoiceComplete(ctx context.Context, id uuid.UUID) bool {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false
	}
	var current gen.BillingInvoice
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		current, err = s.db.Gen(ctx).GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: id})
		return err
	})
	return err == nil && current.Status == string(billing.InvoiceComplete)
}

// newCard is a card just entered, as the invoice's charge saves it.
type newCard struct {
	instrument  charge.FrozenInstrument
	token       string
	billing     *payments.NewCardBilling
	card        *models.Card
	fingerprint string
	reuse       bool
	// saved is the customer's own method for the card, when they saved it
	// before: the invoice charges that instead, presenting collected when a
	// custodian holds it.
	saved     uuid.UUID
	collected string
}

func (s *InvoiceAttemptService) payInvoice(ctx context.Context, in InvoicePayInput) (InvoiceCharge, error) {
	sale := s.saleService()
	purchase := s.purchaseService
	if sale == nil || sale.Intents == nil || s.invoices == nil || purchase == nil {
		return InvoiceCharge{}, errors.New("invoice payment is unavailable")
	}
	if err := s.requireProviderWrites(ctx); err != nil {
		return InvoiceCharge{}, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return InvoiceCharge{}, err
	}
	invoice := in.Invoice
	attemptID := InvoiceAttemptID(mid.UUID(), invoice.ID, in.Key)
	fingerprint := invoiceSaleFingerprint(invoice.ID, in, in.Key)
	operationKey := NMISaleIdempotencyKey("order:" + attemptID.String())
	owns := func(intent gen.BillingProviderIntent) error {
		p, err := payments.DecodeNMISalePayload(intent)
		if err != nil {
			return err
		}
		if p.InvoiceID != invoice.ID || p.InvoiceAttemptID != attemptID || p.UserID != invoice.CustomerID.String() || p.RequestFingerprint != fingerprint {
			return fmt.Errorf("%w: %w", ErrInvoiceAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return nil
	}
	prior, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, operationKey)
	switch {
	case err == nil:
		if err := owns(prior); err != nil {
			return InvoiceCharge{}, err
		}
		intent, err := sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(prior), owns)
		if err != nil {
			return InvoiceCharge{}, err
		}
		return s.afterInvoiceCharge(ctx, invoice, attemptID, intent)
	case !db.IsNotFound(err):
		return InvoiceCharge{}, err
	}

	options, err := s.CustomerInvoiceOptions(ctx, invoice.CustomerID, in.Prices, in.Products)
	if err != nil {
		return InvoiceCharge{}, err
	}
	var chosen *InvoiceOption
	var card *newCard
	methodID := in.PaymentMethodID
	// collected is a custodian's fresh collection of a card the customer
	// already holds: the charge presents it on that PSP.
	var collected string
	if in.Card != nil {
		if chosen, err = NewCardOption(options, in.Card.PSPID); err != nil {
			return InvoiceCharge{}, err
		}
		if card, err = s.prepareNewCard(ctx, purchase, sale, in, *chosen); err != nil {
			return InvoiceCharge{}, err
		}
		if card.saved != uuid.Nil {
			methodID, collected, card = card.saved, card.collected, nil
		}
	}
	if card == nil {
		method, err := s.paymentMethodService.ValidatePaymentMethodOperation(ctx, methodID, in.User.ID)
		if err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) || errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied) {
				return InvoiceCharge{}, fmt.Errorf("%w: %w", ErrPaymentMethodStale, err)
			}
			return InvoiceCharge{}, err
		}
		if method.ParkReason != "" || method.Status != paymentmethods.StatusActive || (method.Rail != models.RailNMI && method.Rail != models.RailStripe) {
			return InvoiceCharge{}, fmt.Errorf("%w: the payment method cannot pay this invoice", ErrPaymentMethodStale)
		}
		// A HyperSwitch-held card pays an invoice that opens a subscription
		// (its proxy takes no one-time card-on-file charge); a Basis Theory
		// card pays after collection, or by saved platform consent. Only platform
		// custody supports recurring invoices through the proxy.
		heldElsewhere := method.Custodian != models.CustodianPSP
		switch {
		case !heldElsewhere:
		case method.CustodianID == nil:
			return InvoiceCharge{}, invoices.ErrOptionMissing
		case method.Custodian == models.CustodianHyperSwitch && invoice.HasRecurring() && collected == "":
		case models.BasisTheoryCustody(method.Custodian):
		default:
			return InvoiceCharge{}, invoices.ErrOptionMissing
		}
		if collected == "" {
			chosen = nil
			for i, o := range options {
				if models.BasisTheoryCustody(method.Custodian) {
					if slices.Contains(o.SavedPaymentMethodIDs, methodID) {
						chosen = &options[i]
						break
					}
					continue
				}
				// A held card is charged through the PSP its custodian serves.
				if !rails.SameRail(method.Rail, models.Rail(o.Rail)) || !method.ChargeableOn(o.PSPID) || heldElsewhere && (o.CustodianID == nil || *o.CustodianID != *method.CustodianID) {
					continue
				}
				chosen = &options[i]
				break
			}
			if chosen == nil {
				return InvoiceCharge{}, invoices.ErrOptionMissing
			}
		}
	}
	target, err := purchase.resolveRailTargetForPSP(ctx, chosen.Rail, chosen.PSPID)
	if err != nil {
		return InvoiceCharge{}, err
	}
	if err := sale.requireArmed(ctx, target); err != nil {
		return InvoiceCharge{}, err
	}
	var intent gen.BillingProviderIntent
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: invoice.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: invoice.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		if err := s.releaseLapsedCCBill(ctx, d, &current, now); err != nil {
			return err
		}
		switch {
		case current.CollectionOwner != string(billing.InvoiceCollectionEngine):
			return invoices.ErrNotPayable
		case current.Status == string(billing.InvoiceProcessing) || current.PaymentStatus == string(billing.InvoicePaymentRequiresAction):
			return invoices.ErrInProgress
		case !invoices.Payable(current, now):
			return invoices.ErrNotPayable
		}
		if err := s.invoices.StillBuyable(ctx, q, invoice, now); err != nil {
			return err
		}
		payload := payments.NMISalePayload{
			InvoiceCaptchaSubjects: intents.InvoiceCaptchaSubjects(ctx),
			InvoiceAttemptID:       attemptID, RequestFingerprint: fingerprint, Provider: target.Rail, PSP: target.PSP,
			Amount: current.Total, ListAmount: current.Total, Currency: current.Currency, Description: "Invoice " + billing.InvoiceID(invoice.ID).String(),
			UserID: invoice.CustomerID.String(), PaymentID: uuidutil.NewV7(), AcceptedAt: now, Eligibility: string(EligibilityAllowed), InvoiceID: invoice.ID, Recurring: invoice.HasRecurring(),
		}
		var charged *uuid.UUID
		if card != nil {
			// The charge saves the card: the method exists once it succeeds.
			payload.PaymentMethodID, payload.Instrument, payload.NewCard = uuidutil.NewV7(), card.instrument, true
			payload.Token, payload.Billing, payload.Card, payload.Fingerprint = card.token, card.billing, card.card, card.fingerprint
			payload.Reuse, payload.PurchaseScoped = card.reuse, !payload.Recurring && !card.reuse
		} else {
			row, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: methodID})
			if err != nil {
				return err
			}
			// The customer pays in person: a recurring line opens a recurring
			// agreement, a one-time invoice reuses the card on file; each cites
			// the card's lineage, or this charge stores it. On Stripe an invoice
			// that starts a subscription stores its own agreement
			// (setup_future_usage), as an enrollment does, and Stripe links the
			// rest.
			instrument, agreement := charge.FreezeInstrument(row, chosen.PSPID), charge.AgreementCardOnFile
			if invoice.HasRecurring() {
				agreement = charge.AgreementRecurring
			}
			if models.BasisTheoryCustody(row.Custodian) && collected == "" {
				consent, err := q.GetCitableMandateForShare(ctx, gen.GetCitableMandateForShareParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, PaymentMethodID: methodID, PspID: chosen.PSPID, Kinds: []string{string(charge.AgreementCardOnFile)}})
				if errors.Is(err, pgx.ErrNoRows) {
					return invoices.ErrOptionMissing
				}
				if err != nil {
					return err
				}
				if invoice.HasRecurring() {
					payload.ReuseConsent = mandates.Lineage(consent)
				} else {
					instrument.Mandate = mandates.Lineage(consent)
				}
			} else if !(invoice.HasRecurring() && row.Rail == string(models.RailStripe)) {
				if instrument.Mandate, err = mandates.Citable(ctx, q, mid.UUID(), invoice.CustomerID, methodID, chosen.PSPID, row.Rail, agreement); err != nil {
					return err
				}
			}
			if row.Custodian == models.CustodianHyperSwitch {
				if s.config == nil || s.config.HyperSwitch == (billing.HyperSwitchConfig{}) {
					return errors.New("HyperSwitch custody is not configured")
				}
				binding, err := charge.FreezeHyperSwitchBinding(ctx, q, charge.CustodyOf(d), row, chosen.PSPID, s.config.HyperSwitch.APIBaseURL)
				if err != nil {
					return fmt.Errorf("%w: %w", ErrPaymentMethodStale, err)
				}
				payload.HyperSwitch = &binding
			}
			payload.PaymentMethodID, payload.Instrument, payload.Token, charged = methodID, instrument, collected, &methodID
		}
		custodianID := uuid.Nil
		if models.BasisTheoryCustody(payload.Instrument.Custodian) && payload.Instrument.CustodianID != nil {
			custodianID = *payload.Instrument.CustodianID
		}
		intent, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: target.Rail, PspID: chosen.PSPID, CustodianID: custodianID,
			IntentType: payments.TypeNMISale, Payload: payload, IdempotencyKey: operationKey, NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: "invoice payment", Inline: true})
		if err != nil {
			return err
		}
		state, err := json.Marshal(map[string]any{"operation_id": intent.ID.String()})
		if err != nil {
			return err
		}
		expires := now.Add(invoiceAttemptTTL)
		if current.ExpiresAt != nil && current.ExpiresAt.Before(expires) {
			expires = *current.ExpiresAt
		}
		if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
			Rail: target.Rail, Status: string(models.InvoiceAttemptStatusCreated), Amount: current.Total, Currency: current.Currency, ExpiresAt: expires, RailState: state, PspID: chosen.PSPID, Now: now}); err != nil {
			return err
		}
		n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PaymentMethodID: charged, PspID: chosen.PSPID, Now: now})
		if err == nil && n != 1 {
			err = invoices.ErrNotPayable
		}
		return err
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return InvoiceCharge{}, invoices.ErrInProgress
		}
		return InvoiceCharge{}, err
	}
	intent, err = sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(intent), owns)
	if err != nil {
		return InvoiceCharge{}, err
	}
	return s.afterInvoiceCharge(ctx, invoice, attemptID, intent)
}

// NewCardOption is the PSP a new card's token belongs to: the one named, or
// the only option that takes new cards.
func NewCardOption(options []InvoiceOption, psp uuid.UUID) (*InvoiceOption, error) {
	return pickOption(options, psp, false)
}

// SolanaOption is the Solana PSP that takes the invoice: the one named, or the
// only one.
func SolanaOption(options []InvoiceOption, psp uuid.UUID) (*InvoiceOption, error) {
	return pickOption(options, psp, true)
}

func pickOption(options []InvoiceOption, psp uuid.UUID, solana bool) (*InvoiceOption, error) {
	return pick(options, psp, func(o InvoiceOption) bool { return o.Solana() == solana && models.Rail(o.Rail) != models.RailCCBill })
}

// pick is the option of kind that takes the invoice: the one named, or the
// only one.
func pick(options []InvoiceOption, psp uuid.UUID, kind func(InvoiceOption) bool) (*InvoiceOption, error) {
	var found []*InvoiceOption
	for i := range options {
		if kind(options[i]) && (psp == uuid.Nil || options[i].PSPID == psp) {
			found = append(found, &options[i])
		}
	}
	switch len(found) {
	case 0:
		return nil, invoices.ErrOptionMissing
	case 1:
		return found[0], nil
	}
	return nil, ErrInvoicePSPRequired
}

// prepareNewCard is a new card's instrument, facts and reuse: an NMI token is
// charged as is; a Stripe payment method is read, charged on the customer's
// Stripe customer, and saved by the same PaymentIntent.
func (s *InvoiceAttemptService) prepareNewCard(ctx context.Context, purchase *PurchaseService, sale *NMISaleService, in InvoicePayInput, option InvoiceOption) (*newCard, error) {
	token := strings.TrimSpace(in.Card.Token)
	if token == "" || len(token) > 255 || cardguard.ContainsPAN(token) {
		return nil, apperr.Invalidf("payment.token is a PSP's single-use card token").WithParam("payment.token")
	}
	details, err := s.NewCardBilling(ctx, option, in.Card.Billing)
	if err != nil {
		return nil, err
	}
	var billingCountry string
	if details != nil && details.Address != nil && details.Address.Country != nil {
		billingCountry = *details.Address.Country
	}
	if option.Custodian == models.CustodianBasisTheory || option.Custodian == models.CustodianPlatform {
		return s.prepareBasisTheoryCard(ctx, purchase, in, option, token, billingCountry)
	}
	out := &newCard{instrument: charge.FrozenInstrument{PSPID: option.PSPID, Custodian: models.CustodianPSP}}
	if models.Rail(option.Rail) != models.RailStripe {
		out.token, out.billing = token, newCardBilling(details, in.User)
		out.reuse = mandates.KeepsNewCard(in.Card.Reusable, billingCountry, "")
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if sale.StripeEngines == nil {
		return nil, errors.New("Stripe card sales are not configured")
	}
	psp := option.PSPID
	service, found, err := sale.StripeEngines.ResolveStripeEngineService(ctx, mid.UUID(), &psp)
	if err != nil {
		return nil, err
	}
	if !found || service == nil {
		return nil, errors.New("Stripe sale account unavailable")
	}
	method, err := service.ReadPaymentMethodCard(ctx, token)
	if err != nil {
		return nil, err
	}
	if method == nil || method.Card == nil {
		return nil, fmt.Errorf("%w: Stripe has no card %s", ErrPaymentMethodStale, token)
	}
	customerRef, err := resolveStripeCustomerWith(ctx, purchase.RailCustomerService, service, in.User)
	if err != nil {
		return nil, err
	}
	if customerRef == "" {
		return nil, errors.New("Stripe customer mapping unavailable")
	}
	if method.CustomerID != "" && method.CustomerID != customerRef {
		return nil, fmt.Errorf("%w: the card belongs to another customer", ErrPaymentMethodStale)
	}
	if method.CustomerID != "" {
		row, err := s.db.Gen(ctx).GetPaymentMethodByRailMethodRefForPSP(ctx, gen.GetPaymentMethodByRailMethodRefForPSPParams{MerchantID: mid.UUID(), Rail: string(models.RailStripe), PspID: psp, RailMethodRef: token})
		switch {
		case err == nil && row.CustomerID.String() == in.User.ID:
			out.saved = row.ID
			return out, nil
		case err != nil && !db.IsNotFound(err):
			return nil, err
		}
	}
	if billingCountry == "" {
		billingCountry = method.BillingCountry
	}
	out.instrument.RailCustomerRef, out.instrument.RailMethodRef = customerRef, token
	out.card, out.fingerprint = method.Card, method.Fingerprint
	out.reuse = mandates.KeepsNewCard(in.Card.Reusable, billingCountry, method.CardCountry)
	return out, nil
}

// NewCardBilling applies the merchant's billing fields to a new card's
// details on option (merchantconfig.AdmitBillingDetails): a Stripe card's
// address is Stripe's Payment Element's to collect, so it is kept as sent.
func (s *InvoiceAttemptService) NewCardBilling(ctx context.Context, option InvoiceOption, details *billing.BillingDetails) (*billing.BillingDetails, error) {
	if models.Rail(option.Rail) == models.RailStripe {
		return details, nil
	}
	fields := merchantconfig.DefaultBillingFields
	if s.db != nil {
		var err error
		if fields, err = merchantconfig.NewStore(s.db).BillingFields(ctx); err != nil {
			return nil, err
		}
	}
	return merchantconfig.AdmitBillingDetails(fields, details, "payment.billing_details")
}

// prepareBasisTheoryCard is a card the page's Basis Theory Elements
// collected into a token intent, charged through the custodian's proxy. A
// number the customer's custodian already holds is that card.
func (s *InvoiceAttemptService) prepareBasisTheoryCard(ctx context.Context, purchase *PurchaseService, in InvoicePayInput, option InvoiceOption, token, billingCountry string) (*newCard, error) {
	if option.CustodianID == nil || purchase.CustodianSaleService == nil {
		return nil, errors.New("Basis Theory custody is not configured")
	}
	cfg, _, bt, err := purchase.CustodianSaleService.basisTheory(db.WithCustodianID(ctx, *option.CustodianID), option.PSPID)
	if err != nil {
		return nil, err
	}
	intent, err := bt.GetTokenIntent(ctx, token)
	if basistheory.IsNotFound(err) {
		return nil, apperr.Invalidf("payment.token is no live Basis Theory token intent: collect the card again").WithParam("payment.token")
	}
	if err != nil {
		return nil, err
	}
	if err := validatePlatformCollection(cfg.Custody, intent); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if fingerprint := strings.TrimSpace(intent.Fingerprint); fingerprint != "" {
		held, err := s.db.Gen(ctx).GetPaymentMethodByFingerprint(ctx, gen.GetPaymentMethodByFingerprintParams{MerchantID: mid.UUID(), CustomerID: in.Invoice.CustomerID,
			Custodian: option.Custodian, CustodianID: *option.CustodianID, Fingerprint: fingerprint})
		switch {
		case err == nil && paymentmethods.Chargeable(held):
			return &newCard{saved: held.ID, collected: token}, nil
		case err != nil && !db.IsNotFound(err):
			return nil, err
		}
	}
	out := &newCard{instrument: charge.FrozenInstrument{PSPID: option.PSPID, Custodian: option.Custodian, CustodianID: option.CustodianID},
		token: token, reuse: mandates.KeepsNewCard(in.Card.Reusable, billingCountry, "")}
	if intent.Card != nil {
		card := models.ParseCard(intent.Card.Brand, intent.Card.Last4, "")
		out.card = &card
	}
	return out, nil
}

// newCardBilling is who a new NMI card bills: the details entered with it,
// the customer's email otherwise.
func newCardBilling(d *billing.BillingDetails, user *UserIdentity) *payments.NewCardBilling {
	out := &payments.NewCardBilling{}
	text := func(v *string) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(*v)
	}
	if d != nil {
		out.FirstName, out.LastName = cardholdername.Parts(text(d.Name), "", "")
		out.Email, out.Phone = text(d.Email), text(d.Phone)
		if a := d.Address; a != nil {
			out.Address1, out.Address2, out.City = text(a.Line1), text(a.Line2), text(a.City)
			out.State, out.Zip, out.Country = text(a.State), text(a.PostalCode), text(a.Country)
		}
	}
	if out.Email == "" && user != nil && user.Email != nil {
		out.Email = strings.TrimSpace(*user.Email)
	}
	return out
}

// afterInvoiceCharge records an unfinished charge on its invoice: a Stripe
// payment awaiting authentication requires the customer's action, anything
// else in flight is processing. A finished charge settled the invoice in its
// own transaction.
func (s *InvoiceAttemptService) afterInvoiceCharge(ctx context.Context, invoice *invoices.Invoice, attemptID uuid.UUID, intent gen.BillingProviderIntent) (InvoiceCharge, error) {
	switch intent.Status {
	case intents.StatusSucceeded:
		return InvoiceCharge{}, nil
	case intents.StatusFailedTerminal:
		var evidence map[string]any
		_ = json.Unmarshal(intent.ResultEvidence, &evidence)
		return InvoiceCharge{Failed: true, Declined: evidence["declined"] == true}, nil
	}
	status := billing.InvoicePaymentProcessing
	if intents.EvidenceString(intent, "stripe_payment_intent_id") != "" && authenticationRequired(intent) {
		status = billing.InvoicePaymentRequiresAction
	}
	return InvoiceCharge{}, s.invoices.Pending(ctx, invoice.ID, attemptID, status)
}

func invoiceSaleFingerprint(invoice uuid.UUID, in InvoicePayInput, key string) string {
	payment := in.PaymentMethodID.String()
	if in.Card != nil {
		payment = "token:" + strings.TrimSpace(in.Card.Token)
	}
	sum := sha256.Sum256([]byte(invoice.String() + "\x00" + payment + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// InvoiceOperation is the provider intent of an invoice's attempt.
func (s *InvoiceAttemptService) InvoiceOperation(ctx context.Context, attemptID uuid.UUID) (gen.BillingProviderIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	var state struct {
		OperationID uuid.UUID `json:"operation_id"`
	}
	if err := json.Unmarshal(attempt.RailState, &state); err != nil || state.OperationID == uuid.Nil {
		return gen.BillingProviderIntent{}, errors.New("invoice attempt has no operation")
	}
	return intents.NewStore(s.db).Get(ctx, state.OperationID)
}

// InvoiceNextAction is what the customer completes for an invoice awaiting them:
// Stripe's in-page authentication of the attempt's payment, or the Solana
// Pay transaction request their wallet opens.
func (s *InvoiceAttemptService) InvoiceNextAction(ctx context.Context, invoice *invoices.Invoice, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (*billing.NextAction, error) {
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) || !invoice.AwaitsCustomer() || invoice.AttemptID == nil {
		return nil, nil
	}
	attempt, solana, err := s.invoiceSolanaAttempt(ctx, *invoice.AttemptID)
	switch {
	case err != nil:
		return nil, err
	case solana:
		return s.invoiceSolanaAction(invoice, attempt), nil
	case hostedAttempt(attempt), ccbillAttempt(attempt):
		return invoiceHostedAction(attempt), nil
	}
	purchase := s.purchaseService
	if purchase == nil || resolver == nil {
		return nil, errors.New("invoice authentication is unavailable")
	}
	operation, err := s.InvoiceOperation(ctx, *invoice.AttemptID)
	if err != nil {
		return nil, err
	}
	return purchase.authenticateAction(ctx, operation, principal, resolver)
}

// ConfirmInvoice reads the provider for the invoice's live attempt after the
// customer acted: the verifier settles it, or it stays where it is. Failed
// reports an attempt that ended without moving money.
func (s *InvoiceAttemptService) ConfirmInvoice(ctx context.Context, invoice *invoices.Invoice, principal billingauth.Payer, signature string) (InvoiceCharge, error) {
	if s.invoices == nil {
		return InvoiceCharge{}, errors.New("invoices are unavailable")
	}
	// Past its expiry the invoice closes first, abandoning its challenge or
	// settling money that already moved, as the expiry pass does.
	invoice, err := s.invoices.ExpireIfDue(ctx, invoice)
	if err != nil {
		return InvoiceCharge{}, err
	}
	// A settled invoice answers as it stands, whoever collects it now (a
	// settled enrollment hands its invoice to the provider). Any other invoice
	// is confirmed only while the engine collects it.
	if invoice.Status == string(billing.InvoiceComplete) {
		return InvoiceCharge{}, nil
	}
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return InvoiceCharge{}, invoices.ErrNotPayable
	}
	if err := s.requireEngineInvoice(ctx, invoice); err != nil {
		// Settled since it was read (the poller enrolled it): it answers as
		// it stands.
		if errors.Is(err, invoices.ErrNotPayable) && s.invoiceComplete(ctx, invoice.ID) {
			return InvoiceCharge{}, nil
		}
		return InvoiceCharge{}, err
	}
	if invoice.AttemptID == nil || (!invoice.AwaitsCustomer() && invoice.Status != string(billing.InvoiceProcessing)) {
		return InvoiceCharge{}, nil
	}
	attempt, solana, err := s.invoiceSolanaAttempt(ctx, *invoice.AttemptID)
	switch {
	case err != nil:
		return InvoiceCharge{}, err
	case solana:
		return InvoiceCharge{}, s.confirmInvoiceSolana(ctx, attempt, signature)
	case hostedAttempt(attempt):
		return s.confirmInvoiceHosted(ctx, attempt)
	case ccbillAttempt(attempt):
		return s.confirmInvoiceCCBill(ctx, attempt)
	}
	purchase := s.purchaseService
	if purchase == nil {
		return InvoiceCharge{}, errors.New("invoice confirmation is unavailable")
	}
	operation, err := s.InvoiceOperation(ctx, *invoice.AttemptID)
	if err != nil {
		return InvoiceCharge{}, err
	}
	if operation.Rail == string(models.RailStripe) {
		if _, err := purchase.confirmStripeAuthentication(ctx, operation.ID, principal); err != nil {
			return InvoiceCharge{}, err
		}
	} else if purchase.Intents != nil {
		if _, err := purchase.Intents.VerifyByID(ctx, operation.ID); err != nil {
			return InvoiceCharge{}, err
		}
	}
	current, err := intents.NewStore(s.db).Get(ctx, operation.ID)
	if err != nil {
		return InvoiceCharge{}, err
	}
	return s.afterInvoiceCharge(ctx, invoice, *invoice.AttemptID, current)
}

// invoiceClosedKey marks the sale of an invoice canceled or expired while its
// payment awaited the customer.
const invoiceClosedKey = "invoice_closed"

// AbandonInvoiceAttempt closes the provider side of a closed invoice's attempt
// that awaited the customer: the challenged payment is canceled at the
// provider. A payment that completed first settles the invoice late instead.
func (s *InvoiceAttemptService) AbandonInvoiceAttempt(ctx context.Context, attemptID uuid.UUID) error {
	// A Solana attempt has nothing to cancel: a transfer landing after the
	// invoice closed is recorded for review.
	attempt, solana, err := s.invoiceSolanaAttempt(ctx, attemptID)
	switch {
	case err != nil || solana:
		return err
	case hostedAttempt(attempt):
		return s.abandonInvoiceHosted(ctx, attempt)
	case ccbillAttempt(attempt):
		// Nothing to close at CCBill: a sale reported later settles late.
		return nil
	}
	operation, err := s.InvoiceOperation(ctx, attemptID)
	if err != nil || operation.Status == intents.StatusSucceeded || operation.Status == intents.StatusFailedTerminal {
		return err
	}
	if err := intents.NewStore(s.db).RecordProgress(ctx, operation.ID, map[string]any{invoiceClosedKey: "true"}); err != nil {
		return err
	}
	purchase := s.purchaseService
	if purchase == nil {
		return nil
	}
	if purchase.Intents != nil {
		_, err = purchase.Intents.VerifyByID(ctx, operation.ID)
	}
	return err
}

// invoiceSolanaAttempt is the invoice attempt, and whether it is on Solana.
func (s *InvoiceAttemptService) invoiceSolanaAttempt(ctx context.Context, attemptID uuid.UUID) (gen.BillingInvoiceAttempt, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingInvoiceAttempt{}, false, err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return gen.BillingInvoiceAttempt{}, false, err
	}
	return attempt, attempt.Rail == string(models.RailSolana), nil
}

func uuidOrNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// completeInvoice settles an invoice sale in its completion transaction: the
// invoice is complete (or its late payment refunded), or open again with the
// decline. saved is the method a new card's charge created, nil otherwise.
func (h *NMISaleIntentHandler) completeInvoice(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p payments.NMISalePayload, customer uuid.UUID, receipt *intents.CollectedReceipt, success bool, evidence map[string]any, outcome intents.Outcome, now time.Time, saved *models.PaymentMethod) error {
	if h.Sale.Invoices == nil {
		return errors.New("invoice settlement is unavailable")
	}
	stripe := in.Rail == string(models.RailStripe)
	// The method this sale charged, once it exists: a new card's only when
	// the charge saved it.
	var method *uuid.UUID
	if !p.NewCard {
		method = &p.PaymentMethodID
	}
	if success {
		metadata := map[string]any{"order_reference": payments.NMISaleOrderReference(in.ID, p.E2ERunID)}
		if stripe {
			metadata["stripe_payment_intent_id"] = receipt.StripeEnginePaymentIntentID()
		}
		storing := receipt.TransactionID()
		if stripe {
			storing = receipt.StripeEnginePaymentIntentID()
		}
		// A saved card's storing one-click charge establishes its card-on-file
		// agreement (a Stripe card has its own from its setup); a new card is
		// kept for one-click buys when the customer keeps it.
		reuse := !p.Recurring && p.Instrument.Mandate == nil && !stripe
		if p.NewCard {
			// A card a custodian could not keep pays without being kept.
			reuse = p.Reuse && saved != nil
			if saved != nil {
				if err := paymentmethods.NewPaymentMethodRepo(d).Create(ctx, saved); err != nil {
					return err
				}
				if _, err := d.Gen(ctx).SetInvoicePaymentMethod(ctx, gen.SetInvoicePaymentMethodParams{MerchantID: in.MerchantID, ID: p.InvoiceID, PaymentMethodID: saved.ID, Now: now}); err != nil {
					return err
				}
				method = &saved.ID
			}
		}
		paid := invoices.Charge{PaymentID: p.PaymentID, AttemptID: p.InvoiceAttemptID, PSPID: *in.PspID, Rail: in.Rail,
			TransactionID: receipt.TransactionID(), Amount: p.Amount, Currency: p.Currency, PurchasedAt: p.AcceptedAt,
			TokenType: saleTokenType(p), Metadata: metadata, Cites: p.Instrument.Mandate, StoringRef: storing, Reuse: reuse}
		switch {
		case method != nil:
			paid.PaymentMethodID = *method
		case p.Card != nil:
			paid.Card = *p.Card
		default:
			paid.Card = receipt.NMICard()
		}
		if _, err := h.Sale.Invoices.Paid(ctx, d, p.InvoiceID, paid); err != nil {
			return err
		}
		// The payment names the subscription an invoice of one bought; so does
		// its attempt.
		payment, err := d.Gen(ctx).GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: in.MerchantID, ID: p.PaymentID})
		if err != nil {
			return err
		}
		if err := recordSaleAttempt(ctx, d, in, p, customer, method, attempts.Attempt{Approved: true, TransactionID: receipt.TransactionID(), PaymentID: &p.PaymentID, SubscriptionID: payment.SubscriptionID}, now); err != nil {
			return err
		}
		// The agreements the charge stored on its card name its attempt.
		if method != nil {
			if err := d.Gen(ctx).LinkMandateStoringAttempt(ctx, gen.LinkMandateStoringAttemptParams{MerchantID: in.MerchantID, PaymentMethodID: *method, PspID: *in.PspID, InitialTransactionID: storing}); err != nil {
				return err
			}
		}
		evidence["transaction_id"], evidence["payment_id"] = receipt.TransactionID(), p.PaymentID.String()
		return nil
	}
	for key, value := range outcome.Evidence {
		evidence[key] = value
	}
	if evidence["declined"] == true {
		code := evidenceText(evidence, "localization_id")
		if code == "" {
			code = evidenceText(evidence, "decline_code")
		}
		answer := decline.Evidence{Code: code, AVS: evidenceText(evidence, "avs_response"), CVV: evidenceText(evidence, "cvv_response"), CardBrand: evidenceText(evidence, "card_brand"), CardLast4: evidenceText(evidence, "card_last4")}
		if err := recordSaleAttempt(ctx, d, in, p, customer, method, attempts.Attempt{Answer: answer, TransactionID: evidenceText(evidence, "decline_transaction_id")}, now); err != nil {
			return err
		}
	}
	return h.Sale.Invoices.Declined(ctx, d, p.InvoiceID, p.InvoiceAttemptID, invoiceFailure(in.Rail, evidence))
}

// newCardMethod is the payment method a new card's successful invoice charge
// saved, read from its holder: the NMI vault the sale created, or the Stripe
// card the PaymentIntent attached. nil when the charge saved nothing.
func (h *NMISaleIntentHandler) newCardMethod(ctx context.Context, in gen.BillingProviderIntent, p payments.NMISalePayload, receipt *intents.CollectedReceipt) (*models.PaymentMethod, error) {
	if !p.SavesCard() || receipt == nil {
		return nil, nil
	}
	customer, err := uuid.Parse(p.UserID)
	if err != nil {
		return nil, err
	}
	if p.Instrument.Custodian == models.CustodianBasisTheory || p.Instrument.Custodian == models.CustodianPlatform {
		return h.basisTheoryMethod(ctx, in, p, customer)
	}
	psp := *in.PspID
	method := &models.PaymentMethod{ID: p.PaymentMethodID, CustomerID: customer, PspID: &psp, Rail: models.Rail(in.Rail), Custodian: models.CustodianPSP}
	if in.Rail == string(models.RailStripe) {
		if p.Card == nil {
			return nil, errors.New("new Stripe card has no reported facts")
		}
		method.RailCustomerRef, method.RailMethodRef, method.Card, method.Fingerprint = p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef, *p.Card, p.Fingerprint
		return method, nil
	}
	vault := receipt.NMICustomerVaultID()
	client, err := h.Sale.nmiClient(db.WithPSPID(ctx, psp), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil || client == nil {
		return nil, fmt.Errorf("NMI account for the new card's vault: %v", err)
	}
	read, found, err := client.GetCustomer(ctx, vault)
	if err != nil || !found {
		return nil, fmt.Errorf("read the new card's vault %s: found=%v: %v", vault, found, err)
	}
	entry := read.PrimaryBilling()
	if entry == nil {
		return nil, fmt.Errorf("vault %s holds no card", vault)
	}
	method.RailCustomerRef, method.RailMethodRef, method.Card = vault, strings.TrimSpace(entry.ID), paymentmethods.VaultedCard(entry.PaymentDetails)
	if b := p.Billing; b != nil {
		if name := cardholdername.Canonical("", b.FirstName, b.LastName); name != "" {
			method.Metadata = map[string]any{"name_on_card": name}
		}
	}
	return method, nil
}

// basisTheoryMethod is the card a Basis Theory sale charged, converted to
// the custodian's durable token. A token intent that expired before its
// conversion leaves the invoice paid with no card kept.
func (h *NMISaleIntentHandler) basisTheoryMethod(ctx context.Context, in gen.BillingProviderIntent, p payments.NMISalePayload, customer uuid.UUID) (*models.PaymentMethod, error) {
	cfg, _, bt, err := h.Sale.Custody.basisTheory(ctx, *in.PspID)
	if err != nil {
		return nil, err
	}
	reference := payments.NMISaleOrderReference(in.ID, p.E2ERunID)
	opts := basistheory.ConvertOpts{IdempotencyKey: "btconv:" + reference}
	if cfg.Custody.Custodian == models.CustodianPlatform {
		opts.Container = cfg.Custody.Container
	}
	token, err := bt.ConvertTokenIntent(ctx, p.Token, opts)
	if basistheory.IsNotFound(err) {
		h.Log.ErrorContext(ctx, "Basis Theory token intent expired before conversion: the invoice is paid with no card kept", "order_reference", reference)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cfg.Custody.Custodian == models.CustodianPlatform && (token.ID == "" || token.Type != "card" || token.TenantID != cfg.Custody.AccountID || len(token.Containers) != 1 || token.Containers[0] != cfg.Custody.Container) {
		return nil, errors.New("platform vault conversion returned a token outside the merchant container")
	}
	method := &models.PaymentMethod{ID: p.PaymentMethodID, CustomerID: customer, Rail: models.RailNMI, RailMethodRef: token.ID, Custodian: p.Instrument.Custodian,
		CustodianID: p.Instrument.CustodianID, Fingerprint: token.Fingerprint, ChargeVia: nmiproxy.ViaPANProxy}
	if token.Card != nil {
		method.Card = models.ParseCard(token.Card.Brand, token.Card.Last4, "")
		if token.Card.ExpirationMonth >= 1 && token.Card.ExpirationMonth <= 12 && token.Card.ExpirationYear >= 2000 {
			method.Card.ExpMonth, method.Card.ExpYear = token.Card.ExpirationMonth, token.Card.ExpirationYear
		}
	}
	return method, nil
}

// provisionNetworkToken gives a new Basis Theory card its network token when
// the custodian provisions them; never load-bearing.
func (h *NMISaleIntentHandler) provisionNetworkToken(ctx context.Context, in gen.BillingProviderIntent, p payments.NMISalePayload, saved *models.PaymentMethod) {
	cfg, _, bt, err := h.Sale.Custody.basisTheory(ctx, *in.PspID)
	if err != nil || !cfg.Custody.NetworkTokens {
		return
	}
	h.Sale.Custody.provisionNetworkToken(ctx, bt, in.MerchantID, saved.ID, saved.RailMethodRef, payments.NMISaleOrderReference(in.ID, p.E2ERunID))
}

func evidenceText(evidence map[string]any, key string) string {
	switch v := evidence[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// invoiceFailure is the customer-facing reason an invoice's charge failed.
func invoiceFailure(rail string, evidence map[string]any) *billing.PaymentFailure {
	reason := billing.DeclineProcessingError
	switch {
	case evidence["declined"] == true:
		code := evidenceText(evidence, "localization_id")
		if code == "" {
			code = evidenceText(evidence, "decline_code")
		}
		if code == "" || code == "0" {
			code = evidenceText(evidence, "response_code")
		}
		// The gateway's AVS and CVV letters name the field to correct.
		reason = decline.ClassifyEvidence(decline.Evidence{Rail: rail, Code: code,
			AVS: evidenceText(evidence, "avs_response"), CVV: evidenceText(evidence, "cvv_response")}).Reason
		if decline.ProviderFault(reason) {
			reason = billing.DeclineProcessingError
		}
	case evidence["duplicate_refused"] == true:
		reason = billing.DeclineDuplicateTransaction
	case evidenceText(evidence, "decline_code") == "authentication_required":
		reason = billing.DeclineAuthenticationRequired
	}
	failure := reason.Failure()
	return &failure
}
