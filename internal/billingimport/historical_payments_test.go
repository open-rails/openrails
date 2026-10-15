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
