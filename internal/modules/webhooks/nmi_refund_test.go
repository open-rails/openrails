package webhooks

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestNMIRefundTime(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	original := &models.Payment{TransactionID: "100001", Currency: "USD", PurchasedAt: now.Add(-24 * time.Hour)}
	for _, tc := range []struct {
		name  string
		edit  func(*nmi.QueryTransaction)
		valid bool
	}{
		{"negative report amount", func(*nmi.QueryTransaction) {}, true},
		{"positive report amount", func(r *nmi.QueryTransaction) { r.Actions[0].Amount = "1.01" }, true},
		{"another refund", func(r *nmi.QueryTransaction) { r.TransactionID = "100003" }, false},
		{"missing original", func(r *nmi.QueryTransaction) { r.OriginalTransactionID = "" }, false},
		{"another original", func(r *nmi.QueryTransaction) { r.OriginalTransactionID = "100004" }, false},
		{"different currency", func(r *nmi.QueryTransaction) { r.Currency = "EUR" }, false},
		{"different amount", func(r *nmi.QueryTransaction) { r.Actions[0].Amount = "-2.02" }, false},
		{"malformed amount", func(r *nmi.QueryTransaction) { r.Actions[0].Amount = "unknown" }, false},
		{"unsuccessful refund", func(r *nmi.QueryTransaction) { r.Actions[0].Success = "0" }, false},
		{"not a refund", func(r *nmi.QueryTransaction) { r.Actions[0].ActionType = "sale" }, false},
		{"no date", func(r *nmi.QueryTransaction) { r.Actions[0].Date = "" }, false},
		{"invalid date", func(r *nmi.QueryTransaction) { r.Actions[0].Date = "20261301120000" }, false},
		{"before original", func(r *nmi.QueryTransaction) {
			r.Actions[0].Date = original.PurchasedAt.Add(-time.Second).Format(nmi.QueryTimeFormat)
		}, false},
		{"future date", func(r *nmi.QueryTransaction) { r.Actions[0].Date = now.Add(time.Second).Format(nmi.QueryTimeFormat) }, false},
		{"multiple refunds", func(r *nmi.QueryTransaction) { r.Actions = append(r.Actions, r.Actions[0]) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := nmi.QueryTransaction{TransactionID: "100002", OriginalTransactionID: original.TransactionID, Currency: "USD",
				Actions: []nmi.QueryAction{{ActionType: "refund", Amount: "-1.01", Success: "1", Date: at.Format(nmi.QueryTimeFormat)}}}
			tc.edit(&r)
			got, err := nmiRefundTime(nmi.TransactionReport{Transactions: []nmi.QueryTransaction{r}}, original, "100002", 1_010_000, now)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, at, got)
			} else {
				require.Error(t, err)
				require.True(t, got.IsZero())
			}
		})
	}
}

type refundAccountReader struct {
	merchantID uuid.UUID
	pspID      *uuid.UUID
	called     bool
}

func (r *refundAccountReader) ResolveNMIClient(_ context.Context, mid uuid.UUID, psp *uuid.UUID) (*nmi.NMIClient, bool, error) {
	r.merchantID, r.pspID, r.called = mid, psp, true
	return nil, false, nil
}

func TestNMIRefundReadsOnlyRoutedAccount(t *testing.T) {
	mid, psp, other := uuid.New(), uuid.New(), uuid.New()
	ctx := db.WithPSPID(merchant.WithID(t.Context(), billing.MerchantID(mid)), psp)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		original *uuid.UUID
		resolves bool
	}{
		{"no merchant", db.WithPSPID(t.Context(), psp), &psp, false},
		{"no routed account", merchant.WithID(t.Context(), billing.MerchantID(mid)), &psp, false},
		{"no original account", ctx, nil, false},
		{"different account", ctx, &other, false},
		{"matching account", ctx, &psp, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &refundAccountReader{}
			s := &NMIWebhookService{NMIResolver: reader}
			_, err := s.recordNMIRefund(tc.ctx, &models.Payment{PspID: tc.original}, "100002", 1_010_000)
			require.Error(t, err)
			require.Equal(t, tc.resolves, reader.called)
			if tc.resolves {
				require.Equal(t, mid, reader.merchantID)
				require.NotNil(t, reader.pspID, "never fall back to the default account")
				require.Equal(t, psp, *reader.pspID)
			}
		})
	}
}
