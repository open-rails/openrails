package checkout

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
	"github.com/open-rails/openrails/internal/modules/orders"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A Solana subscription is bought on an order of its one recurring line.
// Paying opens an attempt holding the price's plan terms and a reference; the
// wallet asks /v1/orders/{id}/solana-pay for the subscribe transaction (the
// delegated approval and the first period's pull, merchant co-signed). Its
// landed first payment, found by the poller or named by confirm's signature,
// enrolls the subscription and completes the order in one transaction.

// solanaSubscribeFlow marks a Solana attempt that subscribes.
const solanaSubscribeFlow = "subscribe"

func solanaSubscribes(attempt *models.CheckoutAttempt) bool {
	return getStringField(attempt.RailState, "flow") == solanaSubscribeFlow
}

// startOrderSolanaSubscribe opens the subscribe attempt for the order's
// recurring line of price, paid in the plan's token. replaces is the attempt
// whose offer expired, nil for the order's first.
func (s *CheckoutAttemptService) startOrderSolanaSubscribe(ctx context.Context, order *orders.Order, attemptID, psp uuid.UUID, token string, price *models.Price, replaces uuid.UUID) error {
	if err := s.requireProviderWrites(ctx); err != nil {
		return err
	}
	if err := s.solanaSignerAvailable(ctx); err != nil {
		return err
	}
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
	}
	if len(order.Lines) != 1 || order.Lines[0].Quantity != nil && *order.Lines[0].Quantity != 1 || price == nil || order.Lines[0].Amount != price.Amount {
		return fmt.Errorf("%w: a Solana subscription is one seat of its plan", ErrCheckoutAttemptValidation)
	}
	linked := *price
	if err := s.db.LoadPricePSPBindings(ctx, []*models.Price{&linked}, nil); err != nil {
		return err
	}
	terms, err := parseSolanaPlanTerms(linked.ForPSP(psp).PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}
	if token = strings.ToUpper(strings.TrimSpace(token)); token != "" && token != strings.ToUpper(terms.mintSymbol) {
		return fmt.Errorf("%w: the plan is paid in %s", ErrCheckoutAttemptValidation, terms.mintSymbol)
	}
	rail, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckoutAttemptValidation, err)
	}
	if rail.Solana == nil {
		return fmt.Errorf("%w: solana rail is not configured", ErrCheckoutAttemptValidation)
	}
	cfg, ok := rail.Solana.Tokens[strings.ToUpper(terms.mintSymbol)]
	if !ok {
		return fmt.Errorf("%w: unsupported token %s", ErrCheckoutAttemptValidation, terms.mintSymbol)
	}
	decimals, err := solanamodule.RequireMintDecimals(ctx, s.solanaMints, cfg.Mint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	validUntil := s.now().UTC().Add(defaultCheckoutAttemptTTL)
	state := map[string]any{
		"flow": solanaSubscribeFlow, "token_symbol": strings.ToUpper(terms.mintSymbol), "token_mint": cfg.Mint, "token_decimals": strconv.Itoa(decimals),
		"token_amount": strconv.FormatUint(terms.amount, 10), "quote_expires_at": validUntil.Format(time.RFC3339),
		"plan_id": strconv.FormatUint(terms.planID, 10), "mint_symbol": terms.mintSymbol, "amount_base_units": strconv.FormatUint(terms.amount, 10),
		"period_hours": strconv.FormatUint(terms.period, 10), "plan_created_at": strconv.FormatInt(terms.createdAt, 10),
	}
	return s.openOrderSolana(ctx, order, attemptID, psp, state, validUntil, replaces)
}

// reofferOrderSolanaSubscribe opens a fresh subscribe attempt for an order
// whose offer expired. A wallet that took the old offer may still land it
// until its late window closes: the order waits for it.
func (s *CheckoutAttemptService) reofferOrderSolanaSubscribe(ctx context.Context, order *orders.Order, expired *models.CheckoutAttempt) error {
	if getStringField(expired.RailState, "subscriber_wallet") != "" && expired.ExpiresAt != nil && !s.now().After(*expired.ExpiresAt) {
		return fmt.Errorf("%w: a subscription payment for this order may be in flight; try again shortly", ErrCheckoutAttemptConflict)
	}
	if len(order.Lines) != 1 || order.Lines[0].PriceID == nil {
		return orders.ErrNotPayable
	}
	price, err := s.priceService.GetByID(ctx, *order.Lines[0].PriceID)
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	next := OrderAttemptID(mid.UUID(), order.ID, "requote:"+expired.ID.String())
	err = s.startOrderSolanaSubscribe(ctx, order, next, expired.PspID, getStringField(expired.RailState, "token_symbol"), price, expired.ID)
	if errors.Is(err, orders.ErrInProgress) {
		return nil
	}
	return err
}

// enrollSolanaOrder enrolls the subscription the order's landed first payment
// opened and completes the order, in one transaction. A first payment landing
// late, for an order no longer taking it, or for a slot another membership
// took, is recorded for review.
func (s *CheckoutAttemptService) enrollSolanaOrder(ctx context.Context, attempt *models.CheckoutAttempt, signature string) error {
	if s.solanaEnroll == nil || s.orders == nil || attempt.OrderID == nil || attempt.Reference == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
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
	}
	in.PlanCreatedAt, _ = strconv.ParseInt(getStringField(attempt.RailState, "plan_created_at"), 10, 64)
	review := solanamodule.LateSettlement{UserID: attempt.CustomerID.String(), SessionID: attempt.ID.String(), Currency: models.DerefStr(attempt.Currency),
		Token: in.MintSymbol, TokenAmount: in.AmountBaseUnits, ExpiresAt: validUntil}
	if attempt.Amount != nil {
		review.Amount = *attempt.Amount
	}
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
	ctx = db.WithPSPID(ctx, attempt.PspID)
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: attempt.CustomerID}); err != nil {
			return err
		}
		order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: mid.UUID(), ID: *attempt.OrderID})
		if err != nil {
			return err
		}
		if order.Status == string(billing.OrderComplete) && order.AttemptID != nil && *order.AttemptID == attempt.ID {
			return nil
		}
		if live, err := solanaOrderLive(ctx, d, attempt); err != nil || !live {
			return errors.Join(err, errSolanaOrderClosed)
		}
		// One landed payment settles one attempt, across merchants and PSPs.
		if err := d.RunInTx(ctx, func(ctx context.Context, _ pgx.Tx) error {
			return settlement.ClaimCheckout(ctx, d, attempt.ID, enrolled.Signature)
		}); err != nil {
			return err
		}
		sub := enrolled.Subscription
		payment := uuidutil.NewV7()
		outcome, err := s.orders.Paid(ctx, d, order.ID, orders.Charge{PaymentID: payment, AttemptID: attempt.ID, PSPID: attempt.PspID, Rail: string(models.RailSolana),
			TransactionID: enrolled.Signature, Amount: order.Total, Currency: order.Currency, PurchasedAt: enrolled.PeriodStart,
			Metadata: map[string]any{
				"solana_reference": in.Reference, "solana_subscriber_wallet": sub.SubscriberWallet, "solana_subscription_pda": sub.SubscriptionPDA,
				"solana_token_symbol": in.MintSymbol, "solana_token_mint": sub.Mint, "solana_token_amount": strconv.FormatUint(in.AmountBaseUnits, 10),
				"solana_recipient_wallet": sub.MerchantAddress,
			},
			Enrolled: &orders.Enrolled{RailSubscriptionID: sub.SubscriptionPDA, PeriodStart: enrolled.PeriodStart, PeriodEnd: enrolled.PeriodEnd}})
		if err != nil {
			return err
		}
		if outcome != orders.OutcomeComplete {
			return fmt.Errorf("order %s did not complete from its Solana subscription: %s", order.ID, outcome)
		}
		paid, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: payment})
		if err != nil {
			return err
		}
		if paid.SubscriptionID == nil {
			return fmt.Errorf("order %s names no subscription", order.ID)
		}
		sub.SubscriptionID = *paid.SubscriptionID
		return solanasubs.NewSolanaSubscriptionRepo(d).Upsert(ctx, &sub)
	})
	if subscriptions.IsMembershipSlotTaken(err) || errors.Is(err, errSolanaOrderClosed) {
		return errors.Join(err, solanamodule.RecordOrphanedFirstPull(ctx, s.db, s.now().UTC(), enrolled.Signature, review))
	}
	return err
}

// confirmOrderSolana reads the chain for the order's Solana attempt now. A
// subscription's first payment named by signature enrolls at once; anything
// else is the poller's next read.
func (s *CheckoutAttemptService) confirmOrderSolana(ctx context.Context, row gen.BillingCheckoutAttempt, signature string) error {
	attempt, err := models.CheckoutAttemptFromGen(row)
	if err != nil {
		return err
	}
	if signature == "" || !solanaSubscribes(attempt) {
		return s.pollSolanaOrderNow(ctx, row)
	}
	err = s.enrollSolanaOrder(ctx, attempt, signature)
	var late *recurring.LatePaymentError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, solanamodule.ErrSolanaSubscribePending):
		return s.pollSolanaOrderNow(ctx, row)
	case errors.Is(err, recurring.ErrPaymentUnverified):
		return apperr.Invalidf("signature is not this order's payment").WithParam("signature")
	case errors.As(err, &late):
		return apperr.Conflictf("the payment landed after the order's offer expired; it is held for review")
	case errors.Is(err, settlement.ErrClaimed):
		return apperr.Conflictf("the transaction pays another order")
	case subscriptions.IsMembershipSlotTaken(err), errors.Is(err, errSolanaOrderClosed):
		return apperr.Conflictf("the order no longer takes this payment; it is held for review")
	}
	return err
}

// errSolanaOrderClosed: the order no longer takes the attempt's payment.
var errSolanaOrderClosed = errors.New("solana: the order no longer takes this payment")
