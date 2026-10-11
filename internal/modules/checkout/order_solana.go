package checkout

import (
	"context"
	"encoding/json"
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
	"github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// An order paid from a Solana wallet: a Solana Pay transaction request. Paying
// opens an attempt holding a quote in the token chosen and a reference; the
// order awaits the wallet (requires_action, next_action solana_pay). The
// wallet asks /v1/orders/{id}/solana-pay for the transaction, OpenRails builds
// it for the attempt (a fresh quote and attempt once the quote expired), and
// the poller credits the transfer that lands on the reference: the order
// completes in that transaction.

// OrderSolana pays from a Solana wallet in TokenSymbol, through the Solana PSP
// named (zero: the only one that takes the order).
type OrderSolana struct {
	PSPID       uuid.UUID
	TokenSymbol string
}

// payOrderSolana opens the key's Solana attempt on the order; a key already
// used answers where its attempt stands.
func (s *CheckoutAttemptService) payOrderSolana(ctx context.Context, in OrderPayInput) (OrderCharge, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return OrderCharge{}, err
	}
	token := strings.ToUpper(strings.TrimSpace(in.Solana.TokenSymbol))
	attemptID := OrderAttemptID(mid.UUID(), in.Order.ID, in.Key)
	prior, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	switch {
	case err == nil:
		if prior.Rail != string(models.RailSolana) || prior.OrderID == nil || *prior.OrderID != in.Order.ID || !strings.EqualFold(railStateText(prior.RailState, "token_symbol"), token) {
			return OrderCharge{}, fmt.Errorf("%w: %w", ErrCheckoutAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return OrderCharge{}, nil
	case !db.IsNotFound(err):
		return OrderCharge{}, err
	}
	options, err := s.OrderOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return OrderCharge{}, err
	}
	option, err := SolanaOption(options, in.Solana.PSPID)
	if err != nil {
		return OrderCharge{}, err
	}
	return OrderCharge{}, s.startOrderSolana(ctx, in.Order, attemptID, option.PSPID, token, uuid.Nil)
}

// startOrderSolana quotes the order in token and opens attempt on it, the
// order awaiting the wallet. replaces is the attempt whose quote expired, nil
// for the order's first Solana attempt.
func (s *CheckoutAttemptService) startOrderSolana(ctx context.Context, order *orders.Order, attemptID, psp uuid.UUID, token string, replaces uuid.UUID) error {
	if err := s.requireProviderWrites(ctx); err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	state, quoteExpires, err := s.quoteSolana(ctx, order.Total, order.Currency, token)
	if err != nil {
		return err
	}
	reference, err := solana.GenerateReference()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: order.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: mid.UUID(), ID: order.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		awaiting := current.PaymentStatus == string(billing.OrderPaymentRequiresAction)
		switch {
		case current.Status != string(billing.OrderOpen) && current.Status != string(billing.OrderProcessing), current.ExpiresAt != nil && !current.ExpiresAt.After(now):
			return orders.ErrNotPayable
		case replaces == uuid.Nil && (current.Status == string(billing.OrderProcessing) || awaiting):
			return orders.ErrInProgress
		case replaces != uuid.Nil && (!awaiting || current.AttemptID == nil || *current.AttemptID != replaces):
			return orders.ErrInProgress
		}
		if err := s.orders.StillFree(ctx, q, order, now); err != nil {
			return err
		}
		// The expired attempt leaves first: an order has one live attempt.
		if replaces != uuid.Nil {
			if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: mid.UUID(), ID: replaces, Status: string(models.CheckoutAttemptStatusExpired), Now: now}); err != nil {
				return err
			}
		}
		if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: order.ID,
			Rail: string(models.RailSolana), Status: string(models.CheckoutAttemptStatusRequiresAction), Reference: &reference, Amount: current.Total, Currency: current.Currency,
			ExpiresAt: solanaSettleDeadline(quoteExpires), RailState: raw, PspID: psp, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PspID: psp, Now: now}); err != nil || n != 1 {
			return errors.Join(err, orders.ErrNotPayable)
		}
		n, err := q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PaymentStatus: string(billing.OrderPaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = orders.ErrNotPayable
		}
		return err
	})
}

// quoteSolana prices total in token: what the wallet sends and to whom, until
// quote_expires_at. A mint with a transfer hook is refused: OpenRails builds
// the transfer and resolves no hook accounts.
func (s *CheckoutAttemptService) quoteSolana(ctx context.Context, total int64, currency, token string) (map[string]any, time.Time, error) {
	if s.solanaTransactionService == nil || s.solanaPayService == nil {
		return nil, time.Time{}, fmt.Errorf("%w: solana is unavailable", ErrCheckoutAttemptValidation)
	}
	rail, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %w", ErrCheckoutAttemptValidation, err)
	}
	if token == "" {
		return nil, time.Time{}, fmt.Errorf("%w: token_symbol is required", ErrCheckoutAttemptValidation)
	}
	if rail.Solana == nil {
		return nil, time.Time{}, fmt.Errorf("%w: solana rail is not configured", ErrCheckoutAttemptValidation)
	}
	cfg, ok := rail.Solana.Tokens[token]
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%w: unsupported token %s", ErrCheckoutAttemptValidation, token)
	}
	if err := solanamodule.ValidateQuoteCurrency(currency, token, cfg.Mint); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	if !strings.EqualFold(token, "SOL") {
		if solanamodule.IsNativeSOLMint(cfg.Mint) {
			return nil, time.Time{}, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutAttemptValidation)
		}
		info, err := solanamodule.RequireMintInfo(ctx, s.solanaMintInfo, cfg.Mint)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
		}
		if !info.Hook.IsZero() {
			return nil, time.Time{}, fmt.Errorf("%w: %s has a transfer hook", ErrCheckoutAttemptValidation, token)
		}
	}
	decimals, err := solanamodule.RequireMintDecimals(ctx, s.solanaMints, cfg.Mint)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	quotedAt := s.now().UTC()
	quote, err := solanamodule.CalculateTokenQuote(ctx, token, cfg.Mint, decimals, moneyutil.Micros(total), currency, s.fxProvider, s.priceProvider, quotedAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrCheckoutAttemptValidation, err)
	}
	recipient, err := solanamodule.ResolveRecipientWallet(ctx, s.db, s.config)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	expires := quotedAt.Add(defaultCheckoutAttemptTTL)
	state := map[string]any{"token_symbol": token, "token_mint": cfg.Mint, "token_decimals": strconv.Itoa(decimals), "recipient": recipient}
	if err := setSolanaQuoteState(state, quote.Units, quote.TokenPriceUSD, quote.FXRate, quote.FXCurrency, quote.QuotedAt, expires); err != nil {
		return nil, time.Time{}, err
	}
	return state, expires, nil
}

// orderSolanaAction is what the customer completes for a Solana attempt: the
// Solana Pay transaction request their wallet opens, with the quote it pays.
func (s *CheckoutAttemptService) orderSolanaAction(order *orders.Order, attempt gen.BillingCheckoutAttempt) *billing.NextAction {
	psp := billing.PSPID(attempt.PspID)
	payload := map[string]string{
		"token_symbol":     railStateText(attempt.RailState, "token_symbol"),
		"token_mint":       railStateText(attempt.RailState, "token_mint"),
		"recipient":        railStateText(attempt.RailState, "recipient"),
		"quote_expires_at": railStateText(attempt.RailState, "quote_expires_at"),
	}
	if attempt.Reference != nil {
		payload["reference"] = *attempt.Reference
	}
	if units, err := strconv.ParseUint(railStateText(attempt.RailState, "token_amount"), 10, 64); err == nil {
		if decimals, err := strconv.Atoi(railStateText(attempt.RailState, "token_decimals")); err == nil {
			payload["token_amount"] = solanamodule.FormatBaseUnits(units, decimals)
		}
	}
	out := &billing.NextAction{Type: "solana_pay", PSPID: &psp, Transactions: []string{}, Payload: payload}
	if base := s.getAPIBaseURL(); base != "" {
		link := "solana:" + base + "/v1/orders/" + billing.OrderID(order.ID).String() + "/solana-pay"
		out.URL = &link
	}
	return out
}

// OrderSolanaLabel is what a wallet shows before asking for the order's
// transaction: what the order buys.
func (s *CheckoutAttemptService) OrderSolanaLabel(ctx context.Context, orderID uuid.UUID) (string, error) {
	order, attempt, err := s.solanaOrderAttempt(ctx, orderID)
	if err != nil {
		return "", err
	}
	if attempt.Status == string(models.CheckoutAttemptStatusSucceeded) {
		return "", ErrCheckoutAttemptAlreadyCompleted
	}
	return order.Items(), nil
}

// BuildOrderSolanaTransaction answers a wallet's transaction request for the
// order: the one transaction of its Solana attempt for account, after a fresh
// quote (a new attempt) once the attempt's quote expired with nothing landed.
func (s *CheckoutAttemptService) BuildOrderSolanaTransaction(ctx context.Context, orderID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	order, attempt, err := s.solanaOrderAttempt(ctx, orderID)
	if err != nil {
		return nil, err
	}
	session, err := models.CheckoutAttemptFromGen(attempt)
	if err != nil {
		return nil, err
	}
	if quote, ok := solanaQuoteExpiresAt(session); ok && s.now().After(quote) {
		if session.Reference != nil {
			landed, err := s.solanaTransactionService.ReferenceHasOurTransfer(ctx, *session.Reference, railStateText(attempt.RailState, "recipient"), railStateText(attempt.RailState, "token_mint"), session.ID)
			if errors.Is(err, solanamodule.ErrReferenceUndetermined) {
				return nil, fmt.Errorf("%w: a transfer for this order may be in flight; try again shortly", ErrCheckoutAttemptConflict)
			}
			if err != nil {
				return nil, err
			}
			if landed {
				return nil, fmt.Errorf("%w: a transfer for this order has landed and is being confirmed", ErrCheckoutAttemptConflict)
			}
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		next := OrderAttemptID(mid.UUID(), order.ID, "requote:"+session.ID.String())
		if err := s.startOrderSolana(ctx, order, next, session.PspID, railStateText(attempt.RailState, "token_symbol"), session.ID); err != nil && !errors.Is(err, orders.ErrInProgress) {
			return nil, err
		}
		if _, attempt, err = s.solanaOrderAttempt(ctx, orderID); err != nil {
			return nil, err
		}
	}
	return s.BuildSolanaPayTransaction(ctx, attempt.ID, account)
}

// solanaOrderAttempt is the order and its current attempt, which must be on
// Solana.
func (s *CheckoutAttemptService) solanaOrderAttempt(ctx context.Context, orderID uuid.UUID) (*orders.Order, gen.BillingCheckoutAttempt, error) {
	if s.orders == nil {
		return nil, gen.BillingCheckoutAttempt{}, errors.New("orders are unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, gen.BillingCheckoutAttempt{}, err
	}
	order, err := s.orders.Get(ctx, orderID)
	if db.IsNotFound(err) || errors.Is(err, orders.ErrNotFound) {
		return nil, gen.BillingCheckoutAttempt{}, ErrCheckoutAttemptNotFound
	}
	if err != nil {
		return nil, gen.BillingCheckoutAttempt{}, err
	}
	if order.Status == string(billing.OrderComplete) {
		return nil, gen.BillingCheckoutAttempt{}, ErrCheckoutAttemptAlreadyCompleted
	}
	if !order.AwaitsCustomer() || order.AttemptID == nil {
		return nil, gen.BillingCheckoutAttempt{}, orders.ErrNotPayable
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: *order.AttemptID})
	if err != nil {
		return nil, gen.BillingCheckoutAttempt{}, err
	}
	if attempt.Rail != string(models.RailSolana) {
		return nil, gen.BillingCheckoutAttempt{}, ErrCheckoutAttemptNotSolana
	}
	return order, attempt, nil
}

// solanaOrderLive reports whether a transfer on session still pays its order:
// the order takes payment and session is its current attempt.
func solanaOrderLive(ctx context.Context, d *db.DB, session *models.CheckoutAttempt) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	order, err := d.Gen(ctx).GetOrder(ctx, gen.GetOrderParams{MerchantID: mid.UUID(), ID: *session.OrderID})
	if err != nil {
		return false, err
	}
	live := order.Status == string(billing.OrderOpen) || order.Status == string(billing.OrderProcessing)
	return live && order.AttemptID != nil && *order.AttemptID == session.ID, nil
}

// creditSolanaOrder settles the order a landed transfer paid, in the
// settlement transaction: its payment, completion and fulfilment.
func (s *CheckoutAttemptService) creditSolanaOrder(ctx context.Context, d *db.DB, session *models.CheckoutAttempt, reference string, t solanamodule.ObservedTransfer) (uuid.UUID, error) {
	if s.orders == nil {
		return uuid.Nil, errors.New("order settlement is unavailable")
	}
	landed := s.now().UTC()
	if t.LandedAt != nil {
		landed = *t.LandedAt
	}
	payment := uuidutil.NewV7()
	outcome, err := s.orders.Paid(db.WithPSPID(ctx, session.PspID), d, *session.OrderID, orders.Charge{PaymentID: payment, AttemptID: session.ID, PSPID: session.PspID,
		Rail: string(models.RailSolana), TransactionID: t.Signature, Amount: *session.Amount, Currency: *session.Currency, PurchasedAt: landed,
		Metadata: solanaPaymentMetadata(session, reference, t)})
	if err != nil {
		return uuid.Nil, err
	}
	if outcome != orders.OutcomeComplete {
		return uuid.Nil, fmt.Errorf("order %s did not complete from its Solana transfer: %s", *session.OrderID, outcome)
	}
	return payment, nil
}

// solanaPaymentMetadata is what a Solana payment records of its transfer.
func solanaPaymentMetadata(session *models.CheckoutAttempt, reference string, t solanamodule.ObservedTransfer) map[string]any {
	return map[string]any{
		"solana_reference":    reference,
		"checkout_attempt_id": session.ID.String(),
		"solana_payer_wallet": strings.TrimSpace(t.Payer),
		"solana_token_symbol": getStringField(session.RailState, "token_symbol"),
		"solana_token_mint":   getStringField(session.RailState, "token_mint"),
		"solana_token_amount": strconv.FormatUint(getUint64Field(session.RailState, "token_amount"), 10),
		"solana_received":     strconv.FormatUint(t.Amount, 10),
		"solana_recipient":    getStringField(session.RailState, "recipient"),
	}
}

// pollSolanaOrderNow asks the poller to read the attempt's reference at once:
// the customer says the wallet sent the transfer.
func (s *CheckoutAttemptService) pollSolanaOrderNow(ctx context.Context, attempt gen.BillingCheckoutAttempt) error {
	if attempt.Reference == nil {
		return nil
	}
	ledger := solanamodule.NewPayLedger(s.db)
	if _, err := ledger.Get(ctx, *attempt.Reference); db.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	return ledger.Schedule(ctx, *attempt.Reference, s.now())
}

// railStateText is one text field of an attempt's stored rail state.
func railStateText(raw []byte, key string) string {
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		return ""
	}
	return getStringField(state, key)
}
