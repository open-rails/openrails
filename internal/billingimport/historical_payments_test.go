package billingimport

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func TestHistoricalPaymentIncompleteFactsStayBlocked(t *testing.T) {
	t.Parallel()
	psp := uuid.New()
	resolver := &pspResolver{byKey: map[string]gen.BillingPsp{"nmi\x1fnmi": {ID: psp, Rail: "nmi"}}, fallback: PSPRef{Key: "nmi"}}
	now := time.Now().UTC()
	good := billing.DeclaredHistoricalPayment{SourceID: "valid", Customer: billing.CustomerID(uuid.New()), Rail: "nmi", TransactionID: "12345678", Amount: 4_990_000, Currency: "USD", PaidAt: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name   string
		change func(*billing.DeclaredHistoricalPayment)
	}{
		{"no customer", func(p *billing.DeclaredHistoricalPayment) { p.Customer = billing.CustomerID(uuid.Nil) }},
		{"no transaction", func(p *billing.DeclaredHistoricalPayment) { p.TransactionID = "" }},
		{"invented transaction", func(p *billing.DeclaredHistoricalPayment) { p.TransactionID = "legacy-123" }},
		{"no amount", func(p *billing.DeclaredHistoricalPayment) { p.Amount = 0 }},
		{"negative amount", func(p *billing.DeclaredHistoricalPayment) { p.Amount = -1_000_000 }},
		{"fraction of a cent", func(p *billing.DeclaredHistoricalPayment) { p.Amount++ }},
		{"no currency", func(p *billing.DeclaredHistoricalPayment) { p.Currency = "" }},
		{"no date", func(p *billing.DeclaredHistoricalPayment) { p.PaidAt = time.Time{} }},
		{"future date", func(p *billing.DeclaredHistoricalPayment) { p.PaidAt = now.Add(time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			bad.SourceID = "invalid"
			tc.change(&bad)
			res := Result{Reasons: map[string]string{}}
			planned, err := planHistoricalPayments(resolver, []billing.DeclaredHistoricalPayment{bad, good}, now, &res)
			require.NoError(t, err)
			require.Len(t, planned, 1)
			require.Equal(t, good.SourceID, planned[0].decl.SourceID)
			require.Equal(t, []string{bad.SourceID}, res.Blocked)
			require.NotEmpty(t, res.Reasons[bad.SourceID])
		})
	}
}

func TestHistoricalPaymentUnsupportedProvidersStayBlocked(t *testing.T) {
	t.Parallel()
	for _, rail := range []string{"stripe", "ccbill"} {
		t.Run(rail, func(t *testing.T) {
			resolver := &pspResolver{byKey: map[string]gen.BillingPsp{rail + "\x1f" + rail: {ID: uuid.New(), Rail: rail}}, fallback: PSPRef{Key: rail}}
			now := time.Now().UTC()
			p := billing.DeclaredHistoricalPayment{SourceID: "old-payment", Customer: billing.CustomerID(uuid.New()), Rail: rail, TransactionID: "12345678", Amount: 4_990_000, Currency: "USD", PaidAt: now.Add(-time.Hour)}
			res := Result{Reasons: map[string]string{}}
			planned, err := planHistoricalPayments(resolver, []billing.DeclaredHistoricalPayment{p}, now, &res)
			require.NoError(t, err)
			require.Empty(t, planned)
			require.Equal(t, []string{p.SourceID}, res.Blocked)
			require.Contains(t, res.Reasons[p.SourceID], "only NMI")
		})
	}
}

func TestHistoricalRefundValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	payment := billing.DeclaredHistoricalPayment{Rail: "nmi", TransactionID: "120001", Amount: 4_230_000, Currency: "USD", PaidAt: now.Add(-time.Hour)}
	good := billing.DeclaredHistoricalRefund{TransactionID: "120002", Amount: 1_010_000, RefundedAt: now}
	for _, tc := range []struct {
		name    string
		refunds []billing.DeclaredHistoricalRefund
		valid   bool
	}{
		{"partial", []billing.DeclaredHistoricalRefund{good}, true},
		{"equal partial refunds", []billing.DeclaredHistoricalRefund{good, {TransactionID: "120003", Amount: good.Amount, RefundedAt: now}}, true},
		{"full", []billing.DeclaredHistoricalRefund{{TransactionID: "120002", Amount: payment.Amount, RefundedAt: now}}, true},
		{"duplicate", []billing.DeclaredHistoricalRefund{good, good}, false},
		{"same as sale", []billing.DeclaredHistoricalRefund{{TransactionID: payment.TransactionID, Amount: good.Amount, RefundedAt: now}}, false},
		{"negative", []billing.DeclaredHistoricalRefund{{TransactionID: good.TransactionID, Amount: -good.Amount, RefundedAt: now}}, false},
		{"too much", []billing.DeclaredHistoricalRefund{{TransactionID: good.TransactionID, Amount: payment.Amount + 1_000_000, RefundedAt: now}}, false},
		{"before payment", []billing.DeclaredHistoricalRefund{{TransactionID: good.TransactionID, Amount: good.Amount, RefundedAt: payment.PaidAt.Add(-time.Second)}}, false},
		{"future", []billing.DeclaredHistoricalRefund{{TransactionID: good.TransactionID, Amount: good.Amount, RefundedAt: now.Add(time.Second)}}, false},
		{"subcent", []billing.DeclaredHistoricalRefund{{TransactionID: good.TransactionID, Amount: 1, RefundedAt: now}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := payment
			p.Refunds = tc.refunds
			reason := validateHistoricalRefunds(&p, now)
			if tc.valid {
				require.Empty(t, reason)
			} else {
				require.NotEmpty(t, reason)
			}
		})
	}
}
