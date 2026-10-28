package pspimport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// stripeSource reads a Stripe account: customers with their cards,
// subscriptions in every status, charges and refunds with the invoices they
// paid. Every request goes through the write-blocked stripeapi client.
type stripeSource struct {
	fetcher *reconcile.StripeFetcher
	// billOf is each paid charge's invoice, read once per job run.
	billOf map[string]subscriptions.StripeBill
}

type stripeCustomerJSON struct {
	ID              string            `json:"id"`
	Email           string            `json:"email"`
	Name            string            `json:"name"`
	Created         int64             `json:"created"`
	Metadata        map[string]string `json:"metadata"`
	InvoiceSettings struct {
		DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
	} `json:"invoice_settings"`
}

type stripeCardJSON struct {
	ID   string `json:"id"`
	Card struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

type stripeSubscriptionExtra struct {
	Status               string            `json:"status"`
	StartDate            int64             `json:"start_date"`
	CanceledAt           int64             `json:"canceled_at"`
	EndedAt              int64             `json:"ended_at"`
	Metadata             map[string]string `json:"metadata"`
	DefaultPaymentMethod json.RawMessage   `json:"default_payment_method"`
	Items                struct {
		Data []struct {
			Price struct {
				Recurring *struct {
					Interval      string `json:"interval"`
					IntervalCount int    `json:"interval_count"`
				} `json:"recurring"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// stripeRef reads an expandable field: an id, or an object with one.
func stripeRef(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var obj struct{ ID string }
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}

func stripeTime(unix int64) *time.Time {
	if unix <= 0 {
		return nil
	}
	t := time.Unix(unix, 0).UTC()
	return &t
}

func (s *stripeSource) roster(ctx context.Context) ([]staged, error) {
	var out []staged
	customers, err := s.fetcher.StripeList(ctx, "/v1/customers", time.Time{}, time.Time{}, "", nil)
	if err != nil {
		return nil, fmt.Errorf("stripe customers: %w", err)
	}
	for _, raw := range customers {
		var c stripeCustomerJSON
		if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" {
			return nil, fmt.Errorf("stripe customer is unreadable")
		}
		cards, err := s.fetcher.StripeList(ctx, "/v1/payment_methods", time.Time{}, time.Time{}, c.ID, url.Values{"type": {"card"}})
		if err != nil {
			return nil, fmt.Errorf("stripe cards of %s: %w", c.ID, err)
		}
		out = append(out, stripeCustomer(c, cards))
	}
	subs, raws, err := s.fetcher.StripeRoster(ctx)
	if err != nil {
		return nil, fmt.Errorf("stripe subscriptions: %w", err)
	}
	for i, sub := range subs {
		rec, err := stripeSubscription(sub, raws[i])
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func stripeCustomer(c stripeCustomerJSON, cards []json.RawMessage) staged {
	f := facts{}
	f.add(FactCustomerRef, c.ID)
	f.add(FactEmail, c.Email)
	f.add(FactName, c.Name)
	for k, v := range c.Metadata {
		if name := "metadata." + strings.TrimSpace(k); factName.MatchString(name) {
			f.add(name, v)
		}
	}
	primary := stripeRef(c.InvoiceSettings.DefaultPaymentMethod)
	var data customerData
	for _, raw := range cards {
		var pm stripeCardJSON
		if json.Unmarshal(raw, &pm) != nil || pm.ID == "" {
			continue
		}
		data.Cards = append(data.Cards, cardData{MethodRef: pm.ID, Primary: pm.ID == primary || primary == "" && len(cards) == 1,
			Brand: strings.ToLower(pm.Card.Brand), Last4: pm.Card.Last4, ExpMonth: pm.Card.ExpMonth, ExpYear: pm.Card.ExpYear})
		remote := reconcile.RemotePaymentMethod{RailCustomerRef: c.ID, RailMethodRef: pm.ID, CardLast4: pm.Card.Last4, Email: c.Email}
		if pm.Card.ExpMonth > 0 && pm.Card.ExpYear > 0 {
			remote.CardExpiry = fmt.Sprintf("%02d%02d", pm.Card.ExpMonth, pm.Card.ExpYear%100)
		}
		data.Remote = append(data.Remote, remote)
	}
	rec := staged{kind: billing.PSPImportCustomer, ref: c.ID, facts: f, data: data}
	if t := stripeTime(c.Created); t != nil {
		rec.occurredAt = *t
	}
	return rec
}

func stripeSubscription(sub reconcile.RemoteSubscription, raw json.RawMessage) (staged, error) {
	var x stripeSubscriptionExtra
	if err := json.Unmarshal(raw, &x); err != nil {
		return staged{}, fmt.Errorf("stripe subscription %s is unreadable", sub.RailSubscriptionID)
	}
	f := facts{}
	f.add(FactSubscriptionRef, sub.RailSubscriptionID)
	f.add(FactCustomerRef, sub.CustomerID)
	f.add(FactPlan, sub.PlanID)
	for k, v := range x.Metadata {
		if name := "metadata." + strings.TrimSpace(k); factName.MatchString(name) {
			f.add(name, v)
		}
	}
	remote := sub
	remote.Raw = nil
	data := subscriptionData{Plan: sub.PlanID, Currency: sub.Currency, NextBillingAt: sub.NextBillingAt, MethodRef: stripeRef(x.DefaultPaymentMethod), Evidence: raw,
		Remote: &remote}
	if t := stripeTime(x.StartDate); t != nil {
		data.StartedAt = *t
	} else {
		data.StartedAt = sub.CreatedAt
	}
	remote.CreatedAt = data.StartedAt
	native, err := moneyutil.RailMinorToNative(sub.Currency, moneyutil.Cents(sub.AmountCents))
	if err != nil {
		return staged{}, fmt.Errorf("stripe subscription %s amount: %w", sub.RailSubscriptionID, err)
	}
	data.Amount = native
	if len(x.Items.Data) > 0 && x.Items.Data[0].Price.Recurring != nil {
		r := x.Items.Data[0].Price.Recurring
		switch r.Interval {
		case "day":
			data.IntervalHours = r.IntervalCount * 24
		case "week":
			data.IntervalHours = r.IntervalCount * 7 * 24
		case "month":
			data.IntervalMonths = r.IntervalCount
		case "year":
			data.IntervalMonths = r.IntervalCount * 12
		}
	}
	switch x.Status {
	case "canceled":
		data.Ended, data.UserCanceled, data.EndedAt = true, true, stripeTime(x.CanceledAt)
		if ended := stripeTime(x.EndedAt); ended != nil {
			data.EndedAt = ended
		}
	case "incomplete_expired", "unpaid", "paused":
		data.Ended, data.EndedAt = true, stripeTime(x.EndedAt)
	}
	return staged{kind: billing.PSPImportSubscription, ref: sub.RailSubscriptionID, customerRef: sub.CustomerID, occurredAt: data.StartedAt, facts: f, data: data}, nil
}

func (s *stripeSource) history(ctx context.Context, since, until time.Time) ([]staged, error) {
	if s.billOf == nil {
		if err := s.readBills(ctx); err != nil {
			return nil, err
		}
	}
	charges, err := s.fetcher.StripeList(ctx, "/v1/charges", since, until, "", nil)
	if err != nil {
		return nil, fmt.Errorf("stripe charges: %w", err)
	}
	refunds, err := s.fetcher.StripeList(ctx, "/v1/refunds", since, until, "", nil)
	if err != nil {
		return nil, fmt.Errorf("stripe refunds: %w", err)
	}
	customerOf := map[string]string{}
	for _, raw := range charges {
		var c struct{ ID, Customer string }
		_ = json.Unmarshal(raw, &c)
		customerOf[c.ID] = c.Customer
	}
	var out []staged
	for i, t := range reconcile.StripeCharges(charges, refunds) {
		var typ string
		charge := t.TransactionID
		switch t.Type {
		case reconcile.TransactionTypeSale:
			typ = "sale"
		case reconcile.TransactionTypeDecline:
			typ = "decline"
		case reconcile.TransactionTypeRefund:
			typ = "refund"
			var r struct{ Charge string }
			_ = json.Unmarshal(refunds[i-len(charges)], &r)
			charge = r.Charge
		default:
			continue
		}
		if t.OccurredAt.Before(since) || !t.OccurredAt.Before(until) {
			continue // created[lte] is inclusive; windows are half-open
		}
		native, err := moneyutil.RailMinorToNative(t.Currency, moneyutil.Cents(t.AmountCents))
		if err != nil {
			return nil, fmt.Errorf("stripe transaction %s: %w", t.TransactionID, err)
		}
		remote := t
		data := transactionData{TransactionID: t.TransactionID, Type: typ, Success: t.Success, Amount: native, Currency: t.Currency, OccurredAt: t.OccurredAt, Remote: &remote}
		rec := staged{kind: billing.PSPImportTransaction, ref: t.TransactionID, customerRef: customerOf[charge], occurredAt: t.OccurredAt}
		if bill, ok := s.billOf[charge]; ok {
			if typ == "sale" {
				paid := bill
				remote.StripeBill, remote.SubscriptionID = &paid, bill.SubscriptionID
			}
			rec.subscriptionRef, rec.customerRef = bill.SubscriptionID, bill.CustomerID
			if typ == "sale" {
				invoice, err := declaredInvoice(bill)
				if err != nil {
					return nil, err
				}
				data.StripeInvoice = invoice
			}
		}
		rec.facts = facts{}
		rec.facts.add(FactCustomerRef, rec.customerRef)
		rec.facts.add(FactSubscriptionRef, rec.subscriptionRef)
		rec.data = data
		out = append(out, rec)
	}
	return out, nil
}

// readBills reads every subscription's invoices and indexes them by the
// charges that paid them.
func (s *stripeSource) readBills(ctx context.Context) error {
	s.billOf = map[string]subscriptions.StripeBill{}
	subs, _, err := s.fetcher.StripeRoster(ctx)
	if err != nil {
		return fmt.Errorf("stripe subscriptions: %w", err)
	}
	bills := s.fetcher.Bills()
	for _, sub := range subs {
		if err := bills.VisitSubscriptionBills(ctx, sub.RailSubscriptionID, func(bill subscriptions.StripeBill) error {
			for _, p := range bill.Payments {
				if p.ChargeID != "" {
					s.billOf[p.ChargeID] = bill
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("stripe invoices of %s: %w", sub.RailSubscriptionID, err)
		}
	}
	return nil
}

// declaredInvoice is a bill in the billing import's native units.
func declaredInvoice(b subscriptions.StripeBill) (*billing.DeclaredStripeInvoice, error) {
	native := func(minor int64) (int64, error) {
		return moneyutil.RailMinorToNative(b.Currency, moneyutil.Cents(minor))
	}
	out := &billing.DeclaredStripeInvoice{ID: b.ID, CustomerReference: b.CustomerID, Status: b.Status, Reason: b.Reason, Currency: b.Currency, DueAt: b.DueAt}
	for _, p := range []struct {
		in  int64
		out *int64
	}{{b.Total, &out.Total}, {b.AmountDue, &out.AmountDue}, {b.AmountPaid, &out.AmountPaid}, {b.AmountRemaining, &out.AmountRemaining},
		{b.AmountOffStripe, &out.AmountOffStripe}, {b.StartingBalance, &out.StartingBalance}, {b.PrePaymentCredit, &out.PrePaymentCredit}} {
		v, err := native(p.in)
		if err != nil {
			return nil, fmt.Errorf("stripe invoice %s: %w", b.ID, err)
		}
		*p.out = v
	}
	for _, l := range b.Lines {
		amount, err := native(l.Amount)
		if err != nil {
			return nil, fmt.Errorf("stripe invoice %s: %w", b.ID, err)
		}
		out.Lines = append(out.Lines, billing.DeclaredStripeInvoiceLine{ID: l.ID, PriceReference: l.PriceID, Description: l.Description, Quantity: l.Quantity,
			Amount: amount, PeriodStart: l.PeriodStart, PeriodEnd: l.PeriodEnd})
	}
	for _, p := range b.Payments {
		if p.ChargeID == "" {
			continue
		}
		amount, err := native(p.Amount)
		if err != nil {
			return nil, fmt.Errorf("stripe invoice %s: %w", b.ID, err)
		}
		out.Payments = append(out.Payments, billing.DeclaredStripeInvoicePayment{ID: p.ID, ChargeID: p.ChargeID, PaymentIntentID: p.PaymentIntentID, Amount: amount, PaidAt: p.PaidAt})
	}
	return out, nil
}
