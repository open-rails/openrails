package pspimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// nmiSource reads an NMI gateway: v5 customers (vault ids, billing and
// shipping ids, emails, cards), v5 subscriptions joined with the recurring
// report (order ids, emails) and the transaction report.
type nmiSource struct {
	client *nmi.NMIClient
	// currency prices NMI amounts, which name none.
	currency string
	// now is when the roster is read: a schedule billing before it is past due.
	now func() time.Time
}

const nmiPage = 100

func (s *nmiSource) roster(ctx context.Context) ([]staged, error) {
	var out []staged
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now()
	}
	cursor, seen := "", map[string]bool{}
	for {
		page, err := s.client.ListCustomersPage(ctx, cursor, nmiPage, "")
		if err != nil {
			return nil, fmt.Errorf("nmi customers: %w", err)
		}
		for _, c := range page.Customers {
			rec, err := nmiCustomer(c)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
		if !page.HasMore {
			break
		}
		next := string(page.NextCursor)
		if seen[next] {
			return nil, fmt.Errorf("nmi customers: pagination repeated cursor %s", next)
		}
		seen[next], cursor = true, next
	}
	report, err := s.client.RecurringReport(ctx)
	if err != nil {
		return nil, fmt.Errorf("nmi recurring report: %w", err)
	}
	rows := make(map[string]nmi.RecurringReportRow, len(report))
	for _, r := range report {
		rows[strings.TrimSpace(r.SubscriptionID)] = r
	}
	cursor, seen = "", map[string]bool{}
	for {
		page, err := s.client.ListSubscriptionsPage(ctx, cursor, nmiPage)
		if err != nil {
			return nil, fmt.Errorf("nmi subscriptions: %w", err)
		}
		for _, sub := range page.Subscriptions {
			rec, err := s.subscription(sub, rows[strings.TrimSpace(sub.ID)], now)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
		if !page.HasMore {
			break
		}
		next := string(page.NextCursor)
		if seen[next] {
			return nil, fmt.Errorf("nmi subscriptions: pagination repeated cursor %s", next)
		}
		seen[next], cursor = true, next
	}
	return out, nil
}

func nmiCustomer(c nmi.V5Customer) (staged, error) {
	f := facts{}
	ref := strings.TrimSpace(c.ID)
	f.add(FactCustomerRef, ref)
	var data customerData
	primary := c.PrimaryBilling()
	for i := range c.Billing {
		b := &c.Billing[i]
		f.add(FactBillingID, b.ID)
		f.add(FactEmail, b.Email)
		f.add(FactName, fullName(b.FirstName, b.LastName))
		card := cardData{MethodRef: strings.TrimSpace(b.ID), Primary: b == primary, Last4: last4(b.PaymentDetails.CardNumber)}
		card.Brand = strings.ToLower(strings.TrimSpace(b.PaymentDetails.CardType))
		if card.Brand == "" {
			card.Brand = nmi.CardBrandFromMaskedPAN(b.PaymentDetails.CardNumber)
		}
		card.ExpMonth, card.ExpYear = cardExpiry(b.PaymentDetails.CardExp)
		data.Cards = append(data.Cards, card)
	}
	for _, sh := range c.Shipping {
		f.add(FactShippingID, sh.ID)
		f.add(FactEmail, sh.Email)
		f.add(FactName, fullName(sh.FirstName, sh.LastName))
	}
	for k, v := range c.MerchantDefinedFields {
		if name := "mdf." + strings.TrimSpace(k); factName.MatchString(name) {
			f.add(name, v)
		}
	}
	remote, err := reconcile.NMIRemotePaymentMethods(c)
	if err != nil {
		return staged{}, err
	}
	data.Remote = remote
	return staged{kind: billing.PSPImportCustomer, ref: ref, occurredAt: nmiTime(c.Created), facts: f, data: data}, nil
}

func (s *nmiSource) subscription(sub nmi.V5Subscription, row nmi.RecurringReportRow, now time.Time) (staged, error) {
	ref := strings.TrimSpace(sub.ID)
	vault := strings.TrimSpace(sub.CustomerVaultID)
	f := facts{}
	f.add(FactSubscriptionRef, ref)
	f.add(FactCustomerRef, vault)
	f.add(FactOrderID, row.OrderID, row.PONumber)
	f.add(FactEmail, row.Email)
	f.add(FactName, fullName(row.FirstName, row.LastName))
	data := subscriptionData{Currency: s.currency, StartedAt: nmiTime(sub.StartDate), Paused: nmiFlag(sub.PausedSubscription)}
	var stated *providerobjects.Object
	if sub.Plan != nil {
		if plan := providerobjects.NMIPlan(*sub.Plan); plan.Ref != "" {
			plan.Evidence, _ = json.Marshal(map[string]any{"source": "nmi_subscription_plan", "subscription": ref, "plan": sub.Plan})
			stated = &plan
		}
		data.Plan = strings.TrimSpace(sub.Plan.ID)
		days, _ := strconv.Atoi(strings.TrimSpace(sub.Plan.DayFrequency))
		data.IntervalHours = days * 24
		data.IntervalMonths, _ = strconv.Atoi(strings.TrimSpace(sub.Plan.MonthFrequency))
	}
	f.add(FactPlan, data.Plan)
	if next := nmiTime(sub.NextBillingDate); !next.IsZero() {
		data.NextBillingAt = &next
	}
	minor, err := moneyutil.DecimalToRailMinor(s.currency, sub.DeclaredAmount())
	if err != nil {
		return staged{}, fmt.Errorf("nmi subscription %s amount: %w", ref, err)
	}
	if data.Amount, err = moneyutil.RailMinorToNative(s.currency, minor); err != nil {
		return staged{}, err
	}
	data.Evidence, _ = json.Marshal(map[string]any{"source": "nmi_psp_import", "subscription": sub, "recurring": row})
	remote, err := reconcile.NMIRemoteSubscription(sub, now)
	if err != nil {
		return staged{}, err
	}
	remote.Email, remote.Raw = strings.TrimSpace(row.Email), nil
	data.Remote = &remote
	return staged{kind: billing.PSPImportSubscription, ref: ref, customerRef: vault, occurredAt: data.StartedAt, facts: f, data: data, stated: stated}, nil
}

func (s *nmiSource) history(ctx context.Context, since, until time.Time) ([]staged, error) {
	txns, err := reconcile.NMITransactions(ctx, s.client, since, until)
	if err != nil {
		return nil, fmt.Errorf("nmi transactions: %w", err)
	}
	var out []staged
	for _, t := range txns {
		var typ string
		switch t.Type {
		case reconcile.TransactionTypeSale:
			typ = "sale"
		case reconcile.TransactionTypeDecline:
			typ = "decline"
		case reconcile.TransactionTypeRefund:
			typ = "refund"
		default:
			continue // authorizations and voids moved no money
		}
		native, err := moneyutil.RailMinorToNative(t.Currency, moneyutil.Cents(t.AmountCents))
		if err != nil {
			return nil, fmt.Errorf("nmi transaction %s: %w", t.TransactionID, err)
		}
		f := facts{}
		f.add(FactOrderID, t.OrderID)
		f.add(FactCustomerRef, t.CustomerRef)
		f.add(FactEmail, t.Email)
		f.add(FactSubscriptionRef, t.SubscriptionID)
		// Keep staged refs stable for both separate and combined refund reports.
		ref := t.TransactionID
		if typ == "refund" {
			ref = t.TransactionID + ":refund:" + t.OccurredAt.UTC().Format(time.RFC3339)
		}
		remote := t
		out = append(out, staged{kind: billing.PSPImportTransaction, ref: ref, customerRef: t.CustomerRef, subscriptionRef: t.SubscriptionID,
			occurredAt: t.OccurredAt, facts: f, data: transactionData{TransactionID: t.TransactionID, OriginalTransactionID: t.OriginalTransactionID, Type: typ, Success: t.Success, Amount: native, Currency: strings.ToUpper(t.Currency), OccurredAt: t.OccurredAt.UTC(),
				Remote: &remote}})
	}
	return out, nil
}

func fullName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

func last4(masked string) string {
	masked = strings.TrimSpace(masked)
	if len(masked) < 4 {
		return ""
	}
	tail := masked[len(masked)-4:]
	if _, err := strconv.Atoi(tail); err != nil {
		return ""
	}
	return tail
}

// cardExpiry reads NMI's MMYY.
func cardExpiry(mmyy string) (int, int) {
	mmyy = strings.TrimSpace(mmyy)
	if len(mmyy) != 4 {
		return 0, 0
	}
	month, err1 := strconv.Atoi(mmyy[:2])
	year, err2 := strconv.Atoi(mmyy[2:])
	if err1 != nil || err2 != nil || month < 1 || month > 12 {
		return 0, 0
	}
	return month, 2000 + year
}

func nmiTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func nmiFlag(v any) bool {
	text := strings.TrimSpace(fmt.Sprint(v))
	return text == "1" || strings.EqualFold(text, "true")
}
