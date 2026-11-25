package payment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A Solana subscription is bought on an invoice of its one recurring line.
// Paying opens an attempt holding the price's plan terms and a reference; the
// wallet asks /v1/invoices/{id}/solana-pay for the subscribe transaction (the
// delegated approval and the first period's pull, merchant co-signed). Its
// landed first payment, found by the poller or named by confirm's signature,
// enrolls the subscription and completes the invoice in one transaction.

// solanaSubscribeFlow marks a Solana attempt that subscribes.
const solanaSubscribeFlow = "subscribe"

func solanaSubscribes(attempt *models.InvoiceAttempt) bool {
	return getStringField(attempt.RailState, "flow") == solanaSubscribeFlow
}

// startInvoiceSolanaSubscribe opens the subscribe attempt for the invoice's
// recurring line of price, paid in the plan's token. replaces is the attempt
// whose offer expired, nil for the invoice's first.
func (s *InvoiceAttemptService) startInvoiceSolanaSubscribe(ctx context.Context, invoice *invoices.Invoice, attemptID, psp uuid.UUID, token string, price *models.Price, replaces uuid.UUID) error {
	ctx = db.WithPSPID(ctx, psp)
	if err := s.requireProviderWrites(ctx); err != nil {
		return err
	}
	if err := s.solanaSignerAvailable(ctx); err != nil {
		return err
	}
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, recurring.ErrNoCranker)
	}
	if len(invoice.Lines) != 1 || invoice.Lines[0].Quantity != nil && *invoice.Lines[0].Quantity != 1 || price == nil || invoice.Lines[0].Amount != price.Amount {
		return fmt.Errorf("%w: a Solana subscription is one seat of its plan", ErrInvoiceAttemptValidation)
	}
	linked := *price
	if err := s.db.LoadPricePSPBindings(ctx, []*models.Price{&linked}, nil); err != nil {
		return err
	}
	terms, err := parseSolanaPlanTerms(linked.ForPSP(psp).PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}
	if err := s.solanaPlanOwned(ctx, terms); err != nil {
		return err
	}
	if token = strings.ToUpper(strings.TrimSpace(token)); token != "" && token != strings.ToUpper(terms.mintSymbol) {
		return fmt.Errorf("%w: the plan is paid in %s", ErrInvoiceAttemptValidation, terms.mintSymbol)
	}
	rail, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvoiceAttemptValidation, err)
	}
	if rail.Solana == nil {
		return fmt.Errorf("%w: solana rail is not configured", ErrInvoiceAttemptValidation)
	}
	// The plan pays the treasury it was published with.
	treasury := terms.treasury
	if treasury == "" {
		treasury = rail.AccountID
	}
	cfg, ok := rail.Solana.Tokens[strings.ToUpper(terms.mintSymbol)]
	if !ok {
		return fmt.Errorf("%w: unsupported token %s", ErrInvoiceAttemptValidation, terms.mintSymbol)
	}
	decimals, err := solanamodule.RequireMintDecimals(ctx, s.solanaMints, cfg.Mint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, err)
	}
	validUntil := s.now().UTC().Add(defaultInvoiceAttemptTTL)
	state := map[string]any{
		"flow": solanaSubscribeFlow, "token_symbol": strings.ToUpper(terms.mintSymbol), "token_mint": cfg.Mint, "token_decimals": strconv.Itoa(decimals),
		"token_amount": strconv.FormatUint(terms.amount, 10), "quote_expires_at": validUntil.Format(time.RFC3339),
		"plan_id": strconv.FormatUint(terms.planID, 10), "mint_symbol": terms.mintSymbol, "amount_base_units": strconv.FormatUint(terms.amount, 10),
		"period_hours": strconv.FormatUint(terms.period, 10), "plan_created_at": strconv.FormatInt(terms.createdAt, 10),
		"plan_owner": terms.owner, "treasury": treasury,
	}
	return s.openInvoiceSolana(ctx, invoice, attemptID, psp, state, validUntil, replaces)
}

// reofferInvoiceSolanaSubscribe opens a fresh subscribe attempt for an invoice
// whose offer expired. A wallet that took the old offer may still land it
// until its late window closes: the invoice waits for it.
func (s *InvoiceAttemptService) reofferInvoiceSolanaSubscribe(ctx context.Context, invoice *invoices.Invoice, expired *models.InvoiceAttempt) error {
	if getStringField(expired.RailState, "subscriber_wallet") != "" && expired.ExpiresAt != nil && !s.now().After(*expired.ExpiresAt) {
		return fmt.Errorf("%w: a subscription payment for this invoice may be in flight; try again shortly", ErrInvoiceAttemptConflict)
	}
	if len(invoice.Lines) != 1 || invoice.Lines[0].PriceID == nil {
		return invoices.ErrNotPayable
	}
	price, err := s.priceService.GetByID(ctx, *invoice.Lines[0].PriceID)
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	next := InvoiceAttemptID(mid.UUID(), invoice.ID, "requote:"+expired.ID.String())
	err = s.startInvoiceSolanaSubscribe(ctx, invoice, next, expired.PspID, getStringField(expired.RailState, "token_symbol"), price, expired.ID)
	if errors.Is(err, invoices.ErrInProgress) {
		return nil
	}
	return err
}

// enrollSolanaInvoice enrolls the subscription the invoice's landed first payment
// opened and completes the invoice, in one transaction. A first payment landing
// late, for an invoice no longer taking it, or for a slot another membership
// took, is recorded for review.
func (s *InvoiceAttemptService) enrollSolanaInvoice(ctx context.Context, attempt *models.InvoiceAttempt, signature string) error {
	if s.solanaEnroll == nil || s.invoices == nil || attempt.InvoiceID == nil || attempt.Reference == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrInvoiceAttemptValidation)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	wallet := strings.TrimSpace(getStringField(attempt.RailState, "subscriber_wallet"))
	if wallet == "" {
		// No wallet asked for the transaction yet: nothing can have landed.
		return solanamodule.ErrSolanaSubscribePending
	}
	validUntil, _ := solanaQuoteExpiresAt(attempt)
	in := recurring.EnrollInput{
		MerchantID: mid, SubscriberWallet: wallet, Signature: strings.TrimSpace(signature), Reference: *attempt.Reference, ValidUntil: validUntil,
		PlanID: getUint64Field(attempt.RailState, "plan_id"), MintSymbol: getStringField(attempt.RailState, "mint_symbol"),
		AmountBaseUnits: getUint64Field(attempt.RailState, "amount_base_units"), PeriodHours: getUint64Field(attempt.RailState, "period_hours"),
		Owner: getStringField(attempt.RailState, "plan_owner"), Treasury: getStringField(attempt.RailState, "treasury"),
	}
	in.PlanCreatedAt, _ = strconv.ParseInt(getStringField(attempt.RailState, "plan_created_at"), 10, 64)
	review := solanamodule.LateSettlement{UserID: attempt.CustomerID.String(), SessionID: attempt.ID.String(), Currency: models.DerefStr(attempt.Currency),
		Token: in.MintSymbol, TokenAmount: in.AmountBaseUnits, ExpiresAt: validUntil}
	if attempt.Amount != nil {
		review.Amount = *attempt.Amount
	}
	ctx = db.WithPSPID(ctx, attempt.PspID)
	enrolled, err := s.solanaEnroll.Verify(ctx, in)
	var late *recurring.LatePaymentError
	switch {
	case errors.Is(err, recurring.ErrPaymentNotLanded):
		return solanamodule.ErrSolanaSubscribePending
	case errors.As(err, &late):
		return errors.Join(err, solanamodule.RecordLateSettlement(ctx, s.db, late.LandedAt, in.Reference, in.Signature, review))
	case err != nil:
		return err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: attempt.CustomerID}); err != nil {
			return err
		}
		invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: *attempt.InvoiceID})
		if err != nil {
			return err
		}
		if invoice.Status == string(billing.InvoiceComplete) && invoice.AttemptID != nil && *invoice.AttemptID == attempt.ID {
			return nil
		}
		if live, err := solanaInvoiceLive(ctx, d, attempt, s.now().UTC()); err != nil || !live {
			return errors.Join(err, errSolanaInvoiceClosed)
		}
		// One landed payment settles one attempt, across merchants and PSPs.
		if err := d.RunInTx(ctx, func(ctx context.Context, _ pgx.Tx) error {
			return settlement.ClaimInvoice(ctx, d, attempt.ID, enrolled.Signature)
		}); err != nil {
			return err
		}
		sub := enrolled.Subscription
		payment := uuidutil.NewV7()
		outcome, err := s.invoices.Paid(ctx, d, invoice.ID, invoices.Charge{PaymentID: payment, AttemptID: attempt.ID, PSPID: attempt.PspID, Rail: string(models.RailSolana),
			TransactionID: enrolled.Signature, Amount: invoice.Total, Currency: invoice.Currency, PurchasedAt: enrolled.PeriodStart,
			Metadata: map[string]any{
				"solana_reference": in.Reference, "solana_subscriber_wallet": sub.SubscriberWallet, "solana_subscription_pda": sub.SubscriptionPDA,
				"solana_token_symbol": in.MintSymbol, "solana_token_mint": sub.Mint, "solana_token_amount": strconv.FormatUint(in.AmountBaseUnits, 10),
				"solana_treasury": sub.Treasury,
			},
			Enrolled: &invoices.Enrolled{RailSubscriptionID: sub.SubscriptionPDA, PeriodStart: enrolled.PeriodStart, PeriodEnd: enrolled.PeriodEnd}})
		if err != nil {
			return err
		}
		if outcome != invoices.OutcomeComplete {
			return fmt.Errorf("invoice %s did not complete from its Solana subscription: %s", invoice.ID, outcome)
		}
		paid, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: payment})
		if err != nil {
			return err
		}
		if paid.SubscriptionID == nil {
			return fmt.Errorf("invoice %s names no subscription", invoice.ID)
		}
		sub.SubscriptionID = *paid.SubscriptionID
		return solanasubs.NewSolanaSubscriptionRepo(d).Upsert(ctx, &sub)
	})
	if subscriptions.IsMembershipSlotTaken(err) || errors.Is(err, errSolanaInvoiceClosed) {
		return errors.Join(err, solanamodule.RecordOrphanedFirstPull(ctx, s.db, s.now().UTC(), enrolled.Signature, review))
	}
	return err
}

// confirmInvoiceSolana reads the chain for the invoice's Solana attempt now. A
// subscription's first payment named by signature enrolls at once; anything
// else is the poller's next read.
func (s *InvoiceAttemptService) confirmInvoiceSolana(ctx context.Context, row gen.BillingInvoiceAttempt, signature string) error {
	attempt, err := models.InvoiceAttemptFromGen(row)
	if err != nil {
		return err
	}
	if signature == "" || !solanaSubscribes(attempt) {
		return s.pollSolanaInvoiceNow(ctx, row)
	}
	err = s.enrollSolanaInvoice(ctx, attempt, signature)
	var late *recurring.LatePaymentError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, solanamodule.ErrSolanaSubscribePending):
		return s.pollSolanaInvoiceNow(ctx, row)
	case errors.Is(err, recurring.ErrPaymentUnverified):
		return apperr.Invalidf("signature is not this invoice's payment").WithParam("signature")
	case errors.As(err, &late):
		return apperr.Conflictf("the payment landed after the invoice's offer expired; it is held for review")
	case errors.Is(err, settlement.ErrClaimed):
		return apperr.Conflictf("the transaction pays another invoice")
	case subscriptions.IsMembershipSlotTaken(err), errors.Is(err, errSolanaInvoiceClosed):
		return apperr.Conflictf("the invoice no longer takes this payment; it is held for review")
	}
	return err
}

// errSolanaInvoiceClosed: the invoice no longer takes the attempt's payment.
var errSolanaInvoiceClosed = errors.New("solana: the invoice no longer takes this payment")
