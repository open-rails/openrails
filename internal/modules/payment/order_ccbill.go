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
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// An order of one subscription paid on a CCBill FlexForm: CCBill's page
// takes the card and CCBill bills the subscription. Paying opens an attempt
// whose signed FlexForm link carries the attempt's id (reservationId), and
// the order awaits the customer with a redirect_to_url next action. An order
// has one live CCBill page: paying it again answers that page until it
// lapses. CCBill's NewSaleSuccess for the attempt completes the order and
// opens the provider-billed subscription in one transaction; NewSaleFailure
// declines it. A sale the order no longer takes is never granted: it is left
// for the merchant to refund and cancel at CCBill.

// ccbillFlow marks an order attempt paid on a CCBill FlexForm.
const ccbillFlow = "ccbill_flexform"

// ccbillPageTTL is how long an order waits on its CCBill page before another
// payment may replace it. A sale CCBill reports later still settles.
const ccbillPageTTL = time.Hour

// ccbillPeriodSlack bounds how far past one billing interval CCBill's
// announced first renewal may set the paid period's end.
const ccbillPeriodSlack = 72 * time.Hour

// ErrCCBillIdentityRequired: a FlexForm names the customer by their verified
// email and username.
var ErrCCBillIdentityRequired = apperr.New(http.StatusBadRequest, codeCustomerEmailRequired, "CCBill needs the customer's verified email and username.")

// errCCBillSaleWaits: another payment on the order is unresolved; CCBill
// redelivers the sale.
var errCCBillSaleWaits = errors.New("ccbill sale waits: another payment on the order is unresolved")

var ccbillPageLapsed = &billing.PaymentFailure{Reason: "payment_page_expired", Message: "The payment page expired before you paid."}

func ccbillAttempt(attempt gen.BillingOrderAttempt) bool {
	return attempt.Rail == string(models.RailCCBill) && railStateText(attempt.RailState, "flow") == ccbillFlow
}

func ccbillPageLive(attempt gen.BillingOrderAttempt, now time.Time) bool {
	return ccbillAttempt(attempt) && attempt.Status == string(models.OrderAttemptStatusRequiresAction) && attempt.ExpiresAt != nil && now.Before(*attempt.ExpiresAt)
}

// payOrderCCBill opens the order's CCBill page under attemptID, or answers
// the page already open.
func (s *OrderAttemptService) payOrderCCBill(ctx context.Context, in OrderPayInput, option OrderOption, attemptID uuid.UUID, returnURL string) error {
	order := in.Order
	if len(order.Lines) != 1 || len(in.Prices) != 1 || order.Lines[0].BillingIntervalHours == nil || order.Lines[0].Quantity != nil && *order.Lines[0].Quantity != 1 {
		return fmt.Errorf("%w: a CCBill sale is one seat of one subscription", ErrOrderAttemptValidation)
	}
	linked := *in.Prices[0]
	if err := s.db.LoadPricePSPBindings(ctx, []*models.Price{&linked}, nil); err != nil {
		return err
	}
	price := linked.ForPSP(option.PSPID)
	form, flex, ok := price.GetCCBillFlexForm()
	if !ok {
		return orders.ErrOptionMissing
	}
	rbo, _ := price.GetCCBillRecurringBillingOption()
	if in.User != nil && in.Identify != nil {
		if err := in.Identify(ctx, in.User); err != nil {
			return err
		}
	}
	if in.User == nil || in.User.Email == nil || strings.TrimSpace(*in.User.Email) == "" || strings.TrimSpace(in.User.Username) == "" {
		return ErrCCBillIdentityRequired
	}
	purchase, ok := s.purchaseService.(*PurchaseService)
	if !ok {
		return errors.New("CCBill order payment is unavailable")
	}
	client, err := purchase.resolveCCBillClient(db.WithPSPID(ctx, option.PSPID))
	if err != nil {
		return err
	}
	username := strings.TrimSpace(in.User.Username)
	link, err := client.GenerateFlexFormURL(&ccbill.GenerateFlexFormURLParams{
		Username: username, Email: strings.TrimSpace(*in.User.Email), FlexID: flex, FormName: form,
		ReservationID: billing.OrderAttemptID(attemptID).String(), Currency: order.Currency,
	})
	if err != nil {
		return err
	}
	state, err := json.Marshal(map[string]any{
		"flow": ccbillFlow, "return_url": returnURL, "checkout_url": link.RedirectURL, "flex_id": flex, "form_name": form,
		"recurring_billing_option_id": rbo, "username": username, "interval_hours": strconv.Itoa(int(*order.Lines[0].BillingIntervalHours)),
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
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: order.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: mid.UUID(), ID: order.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		if current.PaymentStatus == string(billing.OrderPaymentRequiresAction) && current.AttemptID != nil {
			live, err := q.GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: *current.AttemptID})
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
		case current.Status == string(billing.OrderProcessing) || current.PaymentStatus == string(billing.OrderPaymentRequiresAction):
			return orders.ErrInProgress
		case !orders.Payable(current, now):
			return orders.ErrNotPayable
		}
		if err := s.orders.StillBuyable(ctx, q, order, now); err != nil {
			return err
		}
		expires := now.Add(ccbillPageTTL)
		if current.ExpiresAt != nil && current.ExpiresAt.Before(expires) {
			expires = *current.ExpiresAt
		}
		if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: order.ID,
			Rail: string(models.RailCCBill), Status: string(models.OrderAttemptStatusRequiresAction), Amount: current.Total, Currency: current.Currency,
			ExpiresAt: expires, RailState: state, PspID: option.PSPID, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PspID: option.PSPID, Now: now}); err != nil || n != 1 {
			return errors.Join(err, orders.ErrNotPayable)
		}
		n, err := q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PaymentStatus: string(billing.OrderPaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = orders.ErrNotPayable
		}
		return err
	})
}

// releaseLapsedCCBill frees an order whose CCBill page lapsed, in d's
// transaction holding the order's lock: the attempt fails as expired and
// the order takes another payment. A sale CCBill reports for it later
// settles as a late sale.
func (s *OrderAttemptService) releaseLapsedCCBill(ctx context.Context, d *db.DB, current *gen.BillingOrder, now time.Time) error {
	if s.orders == nil || current.Status != string(billing.OrderOpen) || current.PaymentStatus != string(billing.OrderPaymentRequiresAction) || current.AttemptID == nil {
		return nil
	}
	q := d.Gen(ctx)
	attempt, err := q.GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: current.MerchantID, ID: *current.AttemptID})
	if err != nil {
		return err
	}
	if !ccbillAttempt(attempt) || ccbillPageLive(attempt, now) {
		return nil
	}
	if err := s.orders.Declined(ctx, d, current.ID, attempt.ID, ccbillPageLapsed); err != nil {
		return err
	}
	next, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: current.MerchantID, ID: current.ID})
	if err != nil {
		return err
	}
	*current = next
	return nil
}

// confirmOrderCCBill answers a customer back from CCBill: the sale settles
// when CCBill reports it, so a live page waits, and a lapsed one frees the
// order.
func (s *OrderAttemptService) confirmOrderCCBill(ctx context.Context, attempt gen.BillingOrderAttempt) (OrderCharge, error) {
	if ccbillPageLive(attempt, s.now().UTC()) || attempt.OrderID == nil {
		return OrderCharge{}, nil
	}
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		current, err := d.Gen(ctx).LockOrder(ctx, gen.LockOrderParams{MerchantID: attempt.MerchantID, ID: *attempt.OrderID})
		if err != nil {
			return err
		}
		return s.releaseLapsedCCBill(ctx, d, &current, s.now().UTC())
	})
	return OrderCharge{Failed: err == nil}, err
}

// SettleOrderCCBill settles the CCBill sale of an order attempt: in one
// transaction the order completes and the subscription CCBill bills opens
// with its access. A redelivered sale changes nothing. A sale the order no
// longer takes (paid otherwise, the membership held, or contradicting the
// attempt) answers CCBillSaleUnhonored and writes nothing; one for an order
// that closed while what it buys is held is recorded and held for refund.
// An unresolved other payment on the order answers a retryable error.
func (s *OrderAttemptService) SettleOrderCCBill(ctx context.Context, attemptID uuid.UUID, sale payments.CCBillSale) error {
	if s.orders == nil {
		return errors.New("order settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return err
	}
	if !ccbillAttempt(attempt) || attempt.OrderID == nil {
		return unhonored("not_ccbill_attempt", "order attempt %s was not paid on a CCBill FlexForm", attemptID)
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
		return fmt.Errorf("order attempt %s has no billing interval", attemptID)
	}
	interval := time.Duration(hours) * time.Hour
	end := start.Add(interval)
	if n := sale.NextRenewalAt; n != nil && n.After(start) {
		end = n.UTC().Truncate(time.Microsecond)
		if limit := start.Add(interval + ccbillPeriodSlack); end.After(limit) {
			end = limit
		}
	}
	paymentID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("openrails:ccbill-sale:"+attemptID.String()+":"+sale.TransactionID))
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
		current, err := q.GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
		if err != nil {
			return err
		}
		ours := order.AttemptID != nil && *order.AttemptID == attemptID
		switch {
		case order.Status == string(billing.OrderComplete):
			if ours && current.TransactionID != nil && *current.TransactionID == sale.TransactionID {
				return nil
			}
			return unhonored("order_already_paid", "order %s is already paid", order.ID)
		case !ours && (order.Status == string(billing.OrderProcessing) || order.PaymentStatus == string(billing.OrderPaymentRequiresAction)):
			return errCCBillSaleWaits
		case !ours && order.Status == string(billing.OrderOpen):
			// The page lapsed or was declined, and nothing else is in flight:
			// the sale pays the order.
			if n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PspID: attempt.PspID, Now: now}); err != nil || n != 1 {
				return errors.Join(err, orders.ErrNotPayable)
			}
		}
		outcome, err := s.orders.Paid(ctx, d, order.ID, orders.Charge{PaymentID: paymentID, AttemptID: attemptID, PSPID: attempt.PspID,
			Rail: string(models.RailCCBill), TransactionID: sale.TransactionID, Amount: order.Total, Currency: order.Currency, PurchasedAt: start,
			Metadata: map[string]any{"ccbill_subscription_id": sale.SubscriptionID, "ccbill_flex_id": sale.FlexID, "order_attempt_id": billing.OrderAttemptID(attemptID).String()},
			Enrolled: &orders.Enrolled{RailSubscriptionID: sale.SubscriptionID, PeriodStart: start, PeriodEnd: end}})
		if subscriptions.IsMembershipSlotTaken(err) {
			return unhonored("membership_held", "the customer already holds the membership order %s buys", order.ID)
		}
		if err != nil || outcome != orders.OutcomeRefunded {
			return err
		}
		// The order closed and what it buys is held meanwhile: the payment
		// is kept for refund, and CCBill's subscription must be canceled.
		return alerting.RecordLedgerRepair(ctx, d, now, alerting.LedgerRepair{
			Provider: string(models.RailCCBill), Operation: "ccbill_sale_after_close", TransactionID: sale.TransactionID,
			CustomerID: attempt.CustomerID.String(), IdempotencyKey: "ccbill_sale_after_close:" + sale.TransactionID,
			OriginalPaymentID: &paymentID, Err: errors.New("cancel the CCBill subscription and refund its sale"),
			Metadata: map[string]any{"rail_subscription_id": sale.SubscriptionID, "order_id": billing.OrderID(order.ID).String(), "order_attempt_id": billing.OrderAttemptID(attemptID).String()},
		})
	})
	if db.IsUniqueViolation(err) {
		return unhonored("duplicate_sale", "CCBill transaction %s already settles another attempt", sale.TransactionID)
	}
	return err
}

// ccbillSaleContradicts is why a sale is not the one its attempt offered:
// another currency, amount, FlexForm or customer.
func ccbillSaleContradicts(attempt gen.BillingOrderAttempt, sale payments.CCBillSale) string {
	if attempt.Amount == nil || attempt.Currency == nil || !strings.EqualFold(*attempt.Currency, sale.Currency) {
		return fmt.Sprintf("billed currency %s is not the order's", sale.Currency)
	}
	minor, err := moneyutil.NativeToRailMinorExact(*attempt.Currency, *attempt.Amount)
	if err != nil {
		return err.Error()
	}
	if int64(minor) != sale.InitialMinor || sale.RecurringMinor != nil && *sale.RecurringMinor != int64(minor) {
		return fmt.Sprintf("billed %d (recurring %v) is not the order's %d", sale.InitialMinor, sale.RecurringMinor, minor)
	}
	flex, form, rbo := railStateText(attempt.RailState, "flex_id"), railStateText(attempt.RailState, "form_name"), railStateText(attempt.RailState, "recurring_billing_option_id")
	switch {
	case sale.FlexID != "" && sale.FlexID != flex, sale.FormName != "" && sale.FormName != form:
		return fmt.Sprintf("FlexForm %s/%s is not the attempt's %s/%s", sale.FlexID, sale.FormName, flex, form)
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

// DeclineOrderCCBill records CCBill's decline of an order attempt's sale:
// the order takes another payment. It answers the attempt's customer, or
// pgx.ErrNoRows when attemptID is no CCBill attempt. A settled attempt is
// left as it is.
func (s *OrderAttemptService) DeclineOrderCCBill(ctx context.Context, attemptID uuid.UUID, code, text string) (uuid.UUID, error) {
	if s.orders == nil {
		return uuid.Nil, errors.New("order settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return uuid.Nil, err
	}
	if !ccbillAttempt(attempt) || attempt.OrderID == nil {
		return uuid.Nil, pgx.ErrNoRows
	}
	if attempt.Status == string(models.OrderAttemptStatusSucceeded) {
		return attempt.CustomerID, nil
	}
	reason := decline.ClassifyEvidence(decline.Evidence{Rail: string(models.RailCCBill), Code: strings.TrimSpace(code), Text: strings.TrimSpace(text)}).Reason
	if decline.ProviderFault(reason) {
		reason = billing.DeclineProcessingError
	}
	failure := reason.Failure()
	return attempt.CustomerID, s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.orders.Declined(ctx, s.db.NewWithPgxTx(tx), *attempt.OrderID, attemptID, &failure)
	})
}
