package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// An invoice of one subscription paid on a CCBill FlexForm: CCBill's page
// takes the card and CCBill bills the subscription. Paying opens an attempt
// whose signed FlexForm link carries the attempt's id (reservationId), and
// the invoice awaits the customer with a redirect_to_url next action. An invoice
// has one live CCBill page: paying it again answers that page until it
// lapses. CCBill's NewSaleSuccess for the attempt completes the invoice and
// opens the provider-billed subscription in one transaction; NewSaleFailure
// declines it. A sale the invoice no longer takes is never granted: it is
// recorded and held for refund when it paid an invoice, else refused, and the
// merchant refunds and cancels it at CCBill.

// ccbillFlow marks an invoice attempt paid on a CCBill FlexForm.
const ccbillFlow = "ccbill_flexform"

// ccbillPageTTL is how long an invoice waits on its CCBill page before another
// payment may replace it. A sale CCBill reports later still settles.
const ccbillPageTTL = time.Hour

// ccbillPeriodSlack bounds how far past one billing interval CCBill's
// announced first renewal may set the paid period's end.
const ccbillPeriodSlack = 72 * time.Hour

// ErrCCBillIdentityRequired: a FlexForm names the customer by their verified
// email and username.
var ErrCCBillIdentityRequired = apperr.New(http.StatusBadRequest, billing.CodeCustomerEmailRequired, "Verify your email and set a username to pay with CCBill.")

// CCBillIdentityMissing is what a FlexForm still needs to name user:
// "verified_email" and "username", each when absent. Empty when it can.
func CCBillIdentityMissing(user *UserIdentity) []string {
	var missing []string
	if user.Email == nil || strings.TrimSpace(*user.Email) == "" {
		missing = append(missing, "verified_email")
	}
	if strings.TrimSpace(user.Username) == "" {
		missing = append(missing, "username")
	}
	return missing
}

// errCCBillSaleWaits: another payment on the invoice is unresolved; CCBill
// redelivers the sale.
var errCCBillSaleWaits = errors.New("ccbill sale waits: another payment on the invoice is unresolved")

var ccbillPageLapsed = &billing.PaymentFailure{Reason: billing.DeclinePaymentPageExpired, Message: "The payment page expired before you paid."}

func ccbillAttempt(attempt gen.BillingInvoiceAttempt) bool {
	return attempt.Rail == string(models.RailCCBill) && railStateText(attempt.RailState, "flow") == ccbillFlow
}

func ccbillPageLive(attempt gen.BillingInvoiceAttempt, now time.Time) bool {
	return ccbillAttempt(attempt) && attempt.Status == string(models.InvoiceAttemptStatusRequiresAction) && attempt.ExpiresAt != nil && now.Before(*attempt.ExpiresAt)
}

// payInvoiceCCBill opens the invoice's CCBill page under attemptID, or answers
// the page already open.
func (s *InvoiceAttemptService) payInvoiceCCBill(ctx context.Context, in InvoicePayInput, option InvoiceOption, attemptID uuid.UUID, returnURL string) error {
	invoice := in.Invoice
	if len(invoice.Lines) != 1 || len(in.Prices) != 1 || invoice.Lines[0].BillingIntervalHours == nil || invoice.Lines[0].Quantity != nil && *invoice.Lines[0].Quantity != 1 {
		return fmt.Errorf("%w: a CCBill sale is one seat of one subscription", ErrInvoiceAttemptValidation)
	}
	linked := *in.Prices[0]
	// A stand-in sells the line of ours on its CCBill provider price's FlexForm.
	if option.PriceID != uuid.Nil {
		stand, err := s.priceService.GetByID(ctx, option.PriceID)
		if err != nil {
			return err
		}
		if !stand.ProviderOwned || stand.ProductID != linked.ProductID || !sameSaleTerms(&linked, stand) {
			return invoices.ErrOptionMissing
		}
		linked = *stand
	}
	if err := s.db.LoadPricePSPBindings(ctx, []*models.Price{&linked}, nil); err != nil {
		return err
	}
	price := linked.ForPSP(option.PSPID)
	flex, ok := price.GetCCBillFlexID()
	if !ok {
		return invoices.ErrOptionMissing
	}
	rbo, _ := price.GetCCBillRecurringBillingOption()
	if in.User != nil && in.Identify != nil {
		if err := in.Identify(ctx, in.User); err != nil {
			return err
		}
	}
	if in.User == nil || len(CCBillIdentityMissing(in.User)) > 0 {
		return ErrCCBillIdentityRequired
	}
	purchase := s.purchaseService
	if purchase == nil {
		return errors.New("CCBill invoice payment is unavailable")
	}
	client, err := purchase.resolveCCBillClient(db.WithPSPID(ctx, option.PSPID))
	if err != nil {
		return err
	}
	username := strings.TrimSpace(in.User.Username)
	link, err := client.GenerateFlexFormURL(&ccbill.GenerateFlexFormURLParams{
		Username: username, Email: strings.TrimSpace(*in.User.Email), FlexID: flex,
		ReservationID: billing.InvoiceAttemptID(attemptID).String(), Currency: invoice.Currency,
	})
	if err != nil {
		return err
	}
	state, err := json.Marshal(map[string]any{
		"flow": ccbillFlow, "return_url": returnURL, "checkout_url": link.RedirectURL, "flex_id": flex,
		"recurring_billing_option_id": rbo, "username": username, "interval_hours": strconv.Itoa(int(*invoice.Lines[0].BillingIntervalHours)),
		"price_id": linked.ID.String(),
	})
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		if current.PaymentStatus == string(billing.InvoicePaymentRequiresAction) && current.AttemptID != nil {
			live, err := q.GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: *current.AttemptID})
			if err != nil {
				return err
			}
			if ccbillPageLive(live, now) && live.PspID == option.PSPID {
				return nil
			}
		}
		if err := s.releaseLapsedCCBill(ctx, d, &current, now); err != nil {
			return err
		}
		switch {
		case current.Status == string(billing.InvoiceProcessing) || current.PaymentStatus == string(billing.InvoicePaymentRequiresAction):
			return invoices.ErrInProgress
		case !invoices.Payable(current, now):
			return invoices.ErrNotPayable
		}
		if err := s.invoices.StillBuyable(ctx, q, invoice, now); err != nil {
			return err
		}
		expires := now.Add(ccbillPageTTL)
		if current.ExpiresAt != nil && current.ExpiresAt.Before(expires) {
			expires = *current.ExpiresAt
		}
		if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
			Rail: string(models.RailCCBill), Status: string(models.InvoiceAttemptStatusRequiresAction), Amount: current.Total, Currency: current.Currency,
			ExpiresAt: expires, RailState: state, PspID: option.PSPID, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PspID: option.PSPID, Now: now}); err != nil || n != 1 {
			return errors.Join(err, invoices.ErrNotPayable)
		}
		n, err := q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PaymentStatus: string(billing.InvoicePaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = invoices.ErrNotPayable
		}
		return err
	})
}

// releaseLapsedCCBill frees an invoice whose CCBill page lapsed, in d's
// transaction holding the invoice's lock: the attempt fails as expired and
// the invoice takes another payment. A sale CCBill reports for it later
// settles as a late sale.
func (s *InvoiceAttemptService) releaseLapsedCCBill(ctx context.Context, d *db.DB, current *gen.BillingInvoice, now time.Time) error {
	if s.invoices == nil || current.Status != string(billing.InvoiceOpen) || current.PaymentStatus != string(billing.InvoicePaymentRequiresAction) || current.AttemptID == nil {
		return nil
	}
	q := d.Gen(ctx)
	attempt, err := q.GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: current.MerchantID, ID: *current.AttemptID})
	if err != nil {
		return err
	}
	if !ccbillAttempt(attempt) || ccbillPageLive(attempt, now) {
		return nil
	}
	if err := s.invoices.Declined(ctx, d, current.ID, attempt.ID, ccbillPageLapsed); err != nil {
		return err
	}
	next, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: current.MerchantID, ID: current.ID})
	if err != nil {
		return err
	}
	*current = next
	return nil
}

// confirmInvoiceCCBill answers a customer back from CCBill: the sale settles
// when CCBill reports it, so a live page waits, and a lapsed one frees the
// invoice.
func (s *InvoiceAttemptService) confirmInvoiceCCBill(ctx context.Context, attempt gen.BillingInvoiceAttempt) (InvoiceCharge, error) {
	if ccbillPageLive(attempt, s.now().UTC()) || attempt.InvoiceID == nil {
		return InvoiceCharge{}, nil
	}
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		current, err := d.Gen(ctx).LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: attempt.MerchantID, ID: *attempt.InvoiceID})
		if err != nil {
			return err
		}
		return s.releaseLapsedCCBill(ctx, d, &current, s.now().UTC())
	})
	return InvoiceCharge{Failed: err == nil}, err
}

// SettleInvoiceCCBill settles the CCBill sale of an invoice attempt: in one
// transaction the invoice completes and the subscription CCBill bills opens
// with its access. A redelivered sale changes nothing. A sale for an invoice
// another payment completed, or that closed while what it buys is held, is
// recorded against the invoice and held for refund, with a repair to cancel
// its subscription at CCBill. One the invoice otherwise no longer takes (the
// membership held, or contradicting the attempt) answers
// CCBillSaleUnhonored and writes nothing. An unresolved other payment on the
// invoice answers a retryable error.
func (s *InvoiceAttemptService) SettleInvoiceCCBill(ctx context.Context, attemptID uuid.UUID, sale payments.CCBillSale) error {
	if s.invoices == nil {
		return errors.New("invoice settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return err
	}
	if !ccbillAttempt(attempt) || attempt.InvoiceID == nil {
		return unhonored("not_ccbill_attempt", "invoice attempt %s was not paid on a CCBill FlexForm", attemptID)
	}
	if psp := db.PSPIDFromContext(ctx); psp != uuid.Nil && psp != attempt.PspID {
		return unhonored("account_mismatch", "the sale came from another CCBill account than attempt %s's", attemptID)
	}
	if why := ccbillSaleContradicts(attempt, sale); why != "" {
		return unhonored("terms_mismatch", "%s", why)
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	start := now
	if sale.SoldAt != nil && sale.SoldAt.Before(now) {
		start = sale.SoldAt.UTC().Truncate(time.Microsecond)
	}
	hours, _ := strconv.Atoi(railStateText(attempt.RailState, "interval_hours"))
	if hours <= 0 {
		return fmt.Errorf("invoice attempt %s has no billing interval", attemptID)
	}
	interval := time.Duration(hours) * time.Hour
	end := start.Add(interval)
	if n := sale.NextRenewalAt; n != nil && n.After(start) {
		end = n.UTC().Truncate(time.Microsecond)
		if limit := start.Add(interval + ccbillPeriodSlack); end.After(limit) {
			end = limit
		}
	}
	paymentID := uuidutil.DerivedV7(uuidutil.TimeOf(attemptID), uuid.NameSpaceURL, "openrails:ccbill-sale:"+attemptID.String()+":"+sale.TransactionID)
	// CCBill bills the subscription on the provider price its FlexForm sold.
	var billedPrice *uuid.UUID
	if id, err := uuid.Parse(railStateText(attempt.RailState, "price_id")); err == nil {
		billedPrice = &id
	}
	ctx = db.WithPSPID(ctx, attempt.PspID)
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
		current, err := q.GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
		if err != nil {
			return err
		}
		ours := invoice.AttemptID != nil && *invoice.AttemptID == attemptID
		charge := invoices.Charge{PaymentID: paymentID, AttemptID: attemptID, PSPID: attempt.PspID,
			Rail: string(models.RailCCBill), TransactionID: sale.TransactionID, Amount: invoice.Total, Currency: invoice.Currency, PurchasedAt: start, Card: sale.Card,
			Metadata: map[string]any{"ccbill_subscription_id": sale.SubscriptionID, "ccbill_flex_id": sale.FlexID, "invoice_attempt_id": billing.InvoiceAttemptID(attemptID).String()}}
		switch {
		case invoice.Status == string(billing.InvoiceComplete):
			if ours && current.TransactionID != nil && *current.TransactionID == sale.TransactionID {
				return nil
			}
			// Another payment completed the invoice (a card or another page
			// after this one lapsed, or an earlier sale on this page).
			recorded, err := s.invoices.PaidAgain(ctx, d, invoice.ID, charge)
			if err != nil || !recorded {
				return err
			}
			return ccbillCancelRepair(ctx, d, now, "ccbill_sale_invoice_paid", attempt, invoice.ID, paymentID, sale)
		case !ours && (invoice.Status == string(billing.InvoiceProcessing) || invoice.PaymentStatus == string(billing.InvoicePaymentRequiresAction)):
			return errCCBillSaleWaits
		case !ours && invoice.Status == string(billing.InvoiceOpen) && !invoices.Expired(invoice, now):
			// The page lapsed or was declined, and nothing else is in flight:
			// the sale pays the invoice.
			if n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PspID: attempt.PspID, Now: now}); err != nil || n != 1 {
				return errors.Join(err, invoices.ErrNotPayable)
			}
		}
		charge.Enrolled = &invoices.Enrolled{RailSubscriptionID: sale.SubscriptionID, PeriodStart: start, PeriodEnd: end, PriceID: billedPrice}
		outcome, err := s.invoices.Paid(ctx, d, invoice.ID, charge)
		if subscriptions.IsMembershipSlotTaken(err) {
			return unhonored("membership_held", "the customer already holds the membership invoice %s buys", invoice.ID)
		}
		if err != nil || outcome != invoices.OutcomeRefunded {
			return err
		}
		// The invoice closed and what it buys is held meanwhile.
		return ccbillCancelRepair(ctx, d, now, "ccbill_sale_after_close", attempt, invoice.ID, paymentID, sale)
	})
	if db.IsUniqueViolation(err) {
		return unhonored("duplicate_sale", "CCBill transaction %s already settles another attempt", sale.TransactionID)
	}
	return err
}

// ccbillCancelRepair raises the repair a sale held for refund needs: CCBill
// bills its subscription until the merchant cancels it there. OpenRails
// neither refunds nor cancels at CCBill.
func ccbillCancelRepair(ctx context.Context, d *db.DB, now time.Time, operation string, attempt gen.BillingInvoiceAttempt, invoiceID, paymentID uuid.UUID, sale payments.CCBillSale) error {
	return alerting.RecordLedgerRepair(ctx, d, now, alerting.LedgerRepair{
		Provider: string(models.RailCCBill), Operation: operation, TransactionID: sale.TransactionID,
		CustomerID: attempt.CustomerID.String(), IdempotencyKey: operation + ":" + sale.TransactionID,
		OriginalPaymentID: &paymentID, Err: errors.New("cancel the CCBill subscription and refund its sale"),
		Metadata: map[string]any{"rail_subscription_id": sale.SubscriptionID, "invoice_id": billing.InvoiceID(invoiceID).String(), "invoice_attempt_id": billing.InvoiceAttemptID(attempt.ID).String()},
	})
}

// ccbillSaleContradicts is why a sale is not the one its attempt offered:
// another currency, amount, FlexForm or customer.
func ccbillSaleContradicts(attempt gen.BillingInvoiceAttempt, sale payments.CCBillSale) string {
	if attempt.Amount == nil || attempt.Currency == nil || !strings.EqualFold(*attempt.Currency, sale.Currency) {
		return fmt.Sprintf("billed currency %s is not the invoice's", sale.Currency)
	}
	minor, err := moneyutil.NativeToRailMinorExact(*attempt.Currency, *attempt.Amount)
	if err != nil {
		return err.Error()
	}
	if int64(minor) != sale.InitialMinor || sale.RecurringMinor != nil && *sale.RecurringMinor != int64(minor) {
		return fmt.Sprintf("billed %d (recurring %v) is not the invoice's %d", sale.InitialMinor, sale.RecurringMinor, minor)
	}
	flex, rbo := railStateText(attempt.RailState, "flex_id"), railStateText(attempt.RailState, "recurring_billing_option_id")
	switch {
	case sale.FlexID != "" && sale.FlexID != flex:
		return fmt.Sprintf("FlexForm %s is not the attempt's %s", sale.FlexID, flex)
	case sale.RecurringBillingOptionID != "" && rbo != "" && sale.RecurringBillingOptionID != rbo:
		return fmt.Sprintf("recurring billing option %s is not the attempt's %s", sale.RecurringBillingOptionID, rbo)
	case sale.FlexID == "" && (sale.RecurringBillingOptionID == "" || rbo == ""):
		return "the sale names no FlexForm"
	case !strings.EqualFold(strings.TrimSpace(sale.Username), railStateText(attempt.RailState, "username")):
		return fmt.Sprintf("username %q is not the attempt's customer", sale.Username)
	}
	return ""
}

func unhonored(reason, format string, args ...any) error {
	return &payments.CCBillSaleUnhonored{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// DeclineInvoiceCCBill records CCBill's decline of an invoice attempt's sale:
// the invoice takes another payment. It answers the attempt's customer, or
// pgx.ErrNoRows when attemptID is no CCBill attempt. A settled attempt is
// left as it is.
func (s *InvoiceAttemptService) DeclineInvoiceCCBill(ctx context.Context, attemptID uuid.UUID, code, text string) (uuid.UUID, error) {
	if s.invoices == nil {
		return uuid.Nil, errors.New("invoice settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return uuid.Nil, err
	}
	if !ccbillAttempt(attempt) || attempt.InvoiceID == nil {
		return uuid.Nil, pgx.ErrNoRows
	}
	if attempt.Status == string(models.InvoiceAttemptStatusSucceeded) {
		return attempt.CustomerID, nil
	}
	reason := decline.ClassifyEvidence(decline.Evidence{Rail: string(models.RailCCBill), Code: strings.TrimSpace(code), Text: strings.TrimSpace(text)}).Reason
	if decline.ProviderFault(reason) {
		reason = billing.DeclineProcessingError
	}
	failure := reason.Failure()
	return attempt.CustomerID, s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.invoices.Declined(ctx, s.db.NewWithPgxTx(tx), *attempt.InvoiceID, attemptID, &failure)
	})
}
