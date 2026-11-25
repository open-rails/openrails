package payment

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
	"github.com/open-rails/openrails/internal/modules/invoices"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// An invoice paid from a Solana wallet: a Solana Pay transaction request. Paying
// opens an attempt holding a quote in the token chosen and a reference; the
// invoice awaits the wallet (requires_action, next_action solana_pay). The
// wallet asks /v1/invoices/{id}/solana-pay for the transaction, OpenRails builds
// it for the attempt (a fresh quote and attempt once the quote expired), and
// the poller credits the transfer that lands on the reference: the invoice
// completes in that transaction.

// InvoiceSolana pays from a Solana wallet in TokenSymbol, through the Solana PSP
// named (zero: the only one that takes the invoice).
type InvoiceSolana struct {
	PSPID       uuid.UUID
	TokenSymbol string
}

// payInvoiceSolana opens the key's Solana attempt on the invoice; a key already
// used answers where its attempt stands.
func (s *InvoiceAttemptService) payInvoiceSolana(ctx context.Context, in InvoicePayInput) (InvoiceCharge, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return InvoiceCharge{}, err
	}
	token := strings.ToUpper(strings.TrimSpace(in.Solana.TokenSymbol))
	attemptID := InvoiceAttemptID(mid.UUID(), in.Invoice.ID, in.Key)
	prior, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	switch {
	case err == nil:
		if prior.Rail != string(models.RailSolana) || prior.InvoiceID == nil || *prior.InvoiceID != in.Invoice.ID || !strings.EqualFold(railStateText(prior.RailState, "token_symbol"), token) {
			return InvoiceCharge{}, fmt.Errorf("%w: %w", ErrInvoiceAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return InvoiceCharge{}, nil
	case !db.IsNotFound(err):
		return InvoiceCharge{}, err
	}
	options, err := s.InvoiceOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return InvoiceCharge{}, err
	}
	option, err := SolanaOption(options, in.Solana.PSPID)
	if err != nil {
		return InvoiceCharge{}, err
	}
	if in.Invoice.HasRecurring() {
		if len(in.Prices) != 1 {
			return InvoiceCharge{}, invoices.ErrOptionMissing
		}
		return InvoiceCharge{}, s.startInvoiceSolanaSubscribe(ctx, in.Invoice, attemptID, option.PSPID, token, in.Prices[0], uuid.Nil)
	}
	return InvoiceCharge{}, s.startInvoiceSolana(ctx, in.Invoice, attemptID, option.PSPID, token, uuid.Nil)
}

// startInvoiceSolana quotes the invoice in token and opens attempt on it, the
// invoice awaiting the wallet. replaces is the attempt whose quote expired, nil
// for the invoice's first Solana attempt.
func (s *InvoiceAttemptService) startInvoiceSolana(ctx context.Context, invoice *invoices.Invoice, attemptID, psp uuid.UUID, token string, replaces uuid.UUID) error {
	if err := s.requireProviderWrites(ctx); err != nil {
		return err
	}
	state, quoteExpires, err := s.quoteSolana(ctx, invoice.Total, invoice.Currency, token)
	if err != nil {
		return err
	}
	return s.openInvoiceSolana(ctx, invoice, attemptID, psp, state, quoteExpires, replaces)
}

// openInvoiceSolana opens attempt on the invoice with its Solana terms and a
// fresh reference, offered until validUntil: the invoice awaits the wallet.
func (s *InvoiceAttemptService) openInvoiceSolana(ctx context.Context, invoice *invoices.Invoice, attemptID, psp uuid.UUID, state map[string]any, validUntil time.Time, replaces uuid.UUID) error {
	mid, err := merchant.Require(ctx)
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
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: invoice.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: invoice.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		if replaces == uuid.Nil {
			if err := s.releaseLapsedCCBill(ctx, s.db.NewWithPgxTx(tx), &current, now); err != nil {
				return err
			}
		}
		awaiting := current.PaymentStatus == string(billing.InvoicePaymentRequiresAction)
		switch {
		case current.CollectionOwner != string(billing.InvoiceCollectionEngine):
			return invoices.ErrNotPayable
		case current.Status != string(billing.InvoiceOpen) && current.Status != string(billing.InvoiceProcessing), current.ExpiresAt != nil && !current.ExpiresAt.After(now):
			return invoices.ErrNotPayable
		case replaces == uuid.Nil && (current.Status == string(billing.InvoiceProcessing) || awaiting):
			return invoices.ErrInProgress
		case replaces != uuid.Nil && (!awaiting || current.AttemptID == nil || *current.AttemptID != replaces):
			return invoices.ErrInProgress
		}
		if err := s.invoices.StillBuyable(ctx, q, invoice, now); err != nil {
			return err
		}
		// The expired attempt leaves first: an invoice has one live attempt.
		if replaces != uuid.Nil {
			if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: mid.UUID(), ID: replaces, Status: string(models.InvoiceAttemptStatusExpired), Now: now}); err != nil {
				return err
			}
		}
		if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
			Rail: string(models.RailSolana), Status: string(models.InvoiceAttemptStatusRequiresAction), Reference: &reference, Amount: current.Total, Currency: current.Currency,
			ExpiresAt: solanaSettleDeadline(validUntil), RailState: raw, PspID: psp, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PspID: psp, Now: now}); err != nil || n != 1 {
			return errors.Join(err, invoices.ErrNotPayable)
		}
		n, err := q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PaymentStatus: string(billing.InvoicePaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = invoices.ErrNotPayable
		}
		return err
	})
}

// quoteSolana prices total in token: what the wallet sends and to whom, until
// quote_expires_at. A mint with a transfer hook is refused: OpenRails builds
// the transfer and resolves no hook accounts.
func (s *InvoiceAttemptService) quoteSolana(ctx context.Context, total int64, currency, token string) (map[string]any, time.Time, error) {
	if s.solanaTransactionService == nil || s.solanaPayService == nil {
		return nil, time.Time{}, fmt.Errorf("%w: solana is unavailable", ErrInvoiceAttemptValidation)
	}
	rail, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %w", ErrInvoiceAttemptValidation, err)
	}
	if token == "" {
		return nil, time.Time{}, fmt.Errorf("%w: token_symbol is required", ErrInvoiceAttemptValidation)
	}
	if rail.Solana == nil {
		return nil, time.Time{}, fmt.Errorf("%w: solana rail is not configured", ErrInvoiceAttemptValidation)
	}
	cfg, ok := rail.Solana.Tokens[token]
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%w: unsupported token %s", ErrInvoiceAttemptValidation, token)
	}
	if err := solanamodule.ValidateQuoteCurrency(currency, token, cfg.Mint); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, err)
	}
	if !strings.EqualFold(token, "SOL") {
		if solanamodule.IsNativeSOLMint(cfg.Mint) {
			return nil, time.Time{}, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrInvoiceAttemptValidation)
		}
		info, err := solanamodule.RequireMintInfo(ctx, s.solanaMintInfo, cfg.Mint)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, err)
		}
		if !info.Hook.IsZero() {
			return nil, time.Time{}, fmt.Errorf("%w: %s has a transfer hook", ErrInvoiceAttemptValidation, token)
		}
	}
	decimals, err := solanamodule.RequireMintDecimals(ctx, s.solanaMints, cfg.Mint)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, err)
	}
	quotedAt := s.now().UTC()
	quote, err := solanamodule.CalculateTokenQuote(ctx, s.Log, token, cfg.Mint, decimals, moneyutil.Micros(total), currency, s.fxProvider, s.priceProvider, quotedAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrInvoiceAttemptValidation, err)
	}
	// The PSP's treasury is the Solana Pay recipient: the payment lands there.
	recipient := strings.TrimSpace(rail.AccountID)
	if recipient == "" {
		return nil, time.Time{}, fmt.Errorf("%w: the Solana PSP has no treasury", ErrInvoiceAttemptValidation)
	}
	expires := quotedAt.Add(defaultInvoiceAttemptTTL)
	state := map[string]any{"token_symbol": token, "token_mint": cfg.Mint, "token_decimals": strconv.Itoa(decimals), "recipient": recipient}
	if err := setSolanaQuoteState(state, quote.Units, quote.TokenPriceUSD, quote.FXRate, quote.FXCurrency, quote.QuotedAt, expires); err != nil {
		return nil, time.Time{}, err
	}
	return state, expires, nil
}

// invoiceSolanaAction is what the customer completes for a Solana attempt: the
// Solana Pay transaction request their wallet opens, with the quote it pays.
func (s *InvoiceAttemptService) invoiceSolanaAction(invoice *invoices.Invoice, attempt gen.BillingInvoiceAttempt) *billing.NextAction {
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
	// A subscription pays its plan's amount every period_hours.
	if period := railStateText(attempt.RailState, "period_hours"); period != "" {
		payload["period_hours"] = period
		delete(payload, "recipient")
	}
	if units, err := strconv.ParseUint(railStateText(attempt.RailState, "token_amount"), 10, 64); err == nil {
		if decimals, err := strconv.Atoi(railStateText(attempt.RailState, "token_decimals")); err == nil {
			payload["token_amount"] = solanamodule.FormatBaseUnits(units, decimals)
		}
	}
	out := &billing.NextAction{Type: "solana_pay", PSPID: &psp, Transactions: []string{}, Payload: payload}
	if base := s.getAPIBaseURL(); base != "" {
		link := "solana:" + base + "/v1/invoices/" + billing.InvoiceID(invoice.ID).String() + "/solana-pay"
		out.URL = &link
	}
	return out
}

// InvoiceSolanaLabel is what a wallet shows before asking for the invoice's
// transaction: what the invoice buys.
func (s *InvoiceAttemptService) InvoiceSolanaLabel(ctx context.Context, invoiceID uuid.UUID) (string, error) {
	invoice, attempt, err := s.solanaInvoiceAttempt(ctx, invoiceID)
	if err != nil {
		return "", err
	}
	if attempt.Status == string(models.InvoiceAttemptStatusSucceeded) {
		return "", ErrInvoiceAttemptAlreadyCompleted
	}
	return invoice.Items(), nil
}

// BuildInvoiceSolanaTransaction answers a wallet's transaction request for the
// invoice: the one transaction of its Solana attempt for account, after a fresh
// quote (a new attempt) once the attempt's quote expired with nothing landed.
func (s *InvoiceAttemptService) BuildInvoiceSolanaTransaction(ctx context.Context, invoiceID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	invoice, attempt, err := s.solanaInvoiceAttempt(ctx, invoiceID)
	if err != nil {
		return nil, err
	}
	session, err := models.InvoiceAttemptFromGen(attempt)
	if err != nil {
		return nil, err
	}
	if quote, ok := solanaQuoteExpiresAt(session); ok && s.now().After(quote) && solanaSubscribes(session) {
		if err := s.reofferInvoiceSolanaSubscribe(ctx, invoice, session); err != nil {
			return nil, err
		}
		if _, attempt, err = s.solanaInvoiceAttempt(ctx, invoiceID); err != nil {
			return nil, err
		}
	} else if ok && s.now().After(quote) {
		if session.Reference != nil {
			landed, err := s.solanaTransactionService.ReferenceHasOurTransfer(ctx, *session.Reference, railStateText(attempt.RailState, "recipient"), railStateText(attempt.RailState, "token_mint"), session.ID)
			if errors.Is(err, solanamodule.ErrReferenceUndetermined) {
				return nil, fmt.Errorf("%w: a transfer for this invoice may be in flight; try again shortly", ErrInvoiceAttemptConflict)
			}
			if err != nil {
				return nil, err
			}
			if landed {
				return nil, fmt.Errorf("%w: a transfer for this invoice has landed and is being confirmed", ErrInvoiceAttemptConflict)
			}
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		next := InvoiceAttemptID(mid.UUID(), invoice.ID, "requote:"+session.ID.String())
		if err := s.startInvoiceSolana(ctx, invoice, next, session.PspID, railStateText(attempt.RailState, "token_symbol"), session.ID); err != nil && !errors.Is(err, invoices.ErrInProgress) {
			return nil, err
		}
		if _, attempt, err = s.solanaInvoiceAttempt(ctx, invoiceID); err != nil {
			return nil, err
		}
	}
	return s.BuildSolanaPayTransaction(ctx, attempt.ID, account)
}

// solanaInvoiceAttempt is the invoice and its current attempt, which must be on
// Solana.
func (s *InvoiceAttemptService) solanaInvoiceAttempt(ctx context.Context, invoiceID uuid.UUID) (*invoices.Invoice, gen.BillingInvoiceAttempt, error) {
	if s.invoices == nil {
		return nil, gen.BillingInvoiceAttempt{}, errors.New("invoices are unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, gen.BillingInvoiceAttempt{}, err
	}
	invoice, err := s.invoices.Get(ctx, invoiceID)
	if db.IsNotFound(err) || errors.Is(err, invoices.ErrNotFound) {
		return nil, gen.BillingInvoiceAttempt{}, ErrInvoiceAttemptNotFound
	}
	if err != nil {
		return nil, gen.BillingInvoiceAttempt{}, err
	}
	if invoice.Status == string(billing.InvoiceComplete) {
		return nil, gen.BillingInvoiceAttempt{}, ErrInvoiceAttemptAlreadyCompleted
	}
	if !invoice.AwaitsCustomer() || invoice.AttemptID == nil || invoices.Expired(invoice.BillingInvoice, s.now().UTC()) {
		return nil, gen.BillingInvoiceAttempt{}, invoices.ErrNotPayable
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: *invoice.AttemptID})
	if err != nil {
		return nil, gen.BillingInvoiceAttempt{}, err
	}
	if attempt.Rail != string(models.RailSolana) {
		return nil, gen.BillingInvoiceAttempt{}, ErrInvoiceAttemptNotSolana
	}
	return invoice, attempt, nil
}

// solanaInvoiceLive reports whether a transfer on session still pays its invoice
// at now: the invoice takes payment, not past its expiry, and session is its
// current attempt.
func solanaInvoiceLive(ctx context.Context, d *db.DB, session *models.InvoiceAttempt, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	invoice, err := d.Gen(ctx).GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: *session.InvoiceID})
	if err != nil {
		return false, err
	}
	live := invoice.Status == string(billing.InvoiceOpen) && !invoices.Expired(invoice, now) || invoice.Status == string(billing.InvoiceProcessing)
	return live && invoice.AttemptID != nil && *invoice.AttemptID == session.ID, nil
}

// creditSolanaInvoice settles the invoice a landed transfer paid, in the
// settlement transaction: its payment, completion and fulfilment.
func (s *InvoiceAttemptService) creditSolanaInvoice(ctx context.Context, d *db.DB, session *models.InvoiceAttempt, reference string, t solanamodule.ObservedTransfer) (uuid.UUID, error) {
	if s.invoices == nil {
		return uuid.Nil, errors.New("invoice settlement is unavailable")
	}
	landed := s.now().UTC()
	if t.LandedAt != nil {
		landed = *t.LandedAt
	}
	payment := uuidutil.NewV7()
	outcome, err := s.invoices.Paid(db.WithPSPID(ctx, session.PspID), d, *session.InvoiceID, invoices.Charge{PaymentID: payment, AttemptID: session.ID, PSPID: session.PspID,
		Rail: string(models.RailSolana), TransactionID: t.Signature, Amount: *session.Amount, Currency: *session.Currency, PurchasedAt: landed,
		Metadata: solanaPaymentMetadata(session, reference, t)})
	if err != nil {
		return uuid.Nil, err
	}
	if outcome != invoices.OutcomeComplete {
		return uuid.Nil, fmt.Errorf("invoice %s did not complete from its Solana transfer: %s", *session.InvoiceID, outcome)
	}
	return payment, nil
}

// solanaPaymentMetadata is what a Solana payment records of its transfer.
func solanaPaymentMetadata(session *models.InvoiceAttempt, reference string, t solanamodule.ObservedTransfer) map[string]any {
	return map[string]any{
		"solana_reference":    reference,
		"invoice_attempt_id":  session.ID.String(),
		"solana_payer_wallet": strings.TrimSpace(t.Payer),
		"solana_token_symbol": getStringField(session.RailState, "token_symbol"),
		"solana_token_mint":   getStringField(session.RailState, "token_mint"),
		"solana_token_amount": strconv.FormatUint(getUint64Field(session.RailState, "token_amount"), 10),
		"solana_received":     strconv.FormatUint(t.Amount, 10),
		"solana_recipient":    getStringField(session.RailState, "recipient"),
	}
}

// pollSolanaInvoiceNow asks the poller to read the attempt's reference at once:
// the customer says the wallet sent the transfer.
func (s *InvoiceAttemptService) pollSolanaInvoiceNow(ctx context.Context, attempt gen.BillingInvoiceAttempt) error {
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
