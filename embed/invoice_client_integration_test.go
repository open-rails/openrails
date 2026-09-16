//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInvoiceClientWorkflowAcrossTransports(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	embedded, err := host.Runtime().Client()
	require.NoError(t, err)
	// Run the same application through socket-free embedding, a hosted engine,
	// and the standalone server's real AuthKit API-key gate.
	for name, client := range map[string]*openrails.Client{"embedded": embedded, "hosted_http": host.Client(), "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			payer := identity.CustomerID(uuid.New())
			mid := dbtest.TestMerchantID
			scoped := merchant.WithID(ctx, mid)
			rt := remote.App().Runtime
			mode := money.BillingModeArrears
			require.NoError(t, rt.DB.RunInMerchantConn(scoped, func(c context.Context) error {
				_, err := rt.MoneyService.UpsertAccountSettings(c, payer, "USD", money.AccountSettingsInput{BillingMode: &mode})
				return err
			}))
			profile := openrails.InvoiceProfileDTO{NetTermsDays: 7, CollectionMethod: "send_invoice", Memo: "defaults"}
			created, err := client.EnsureCustomerInvoiceProfile(ctx, payer.String(), profile)
			require.NoError(t, err)
			require.True(t, created)
			profile.Memo = "operator settings"
			require.NoError(t, client.SetCustomerInvoiceProfile(ctx, payer.String(), profile))
			created, err = client.EnsureCustomerInvoiceProfile(ctx, payer.String(), openrails.InvoiceProfileDTO{NetTermsDays: 30, CollectionMethod: "send_invoice"})
			require.NoError(t, err)
			require.False(t, created)
			got, err := client.GetCustomerInvoiceProfile(ctx, payer.String())
			require.NoError(t, err)
			require.Equal(t, profile, *got)
			// Seed a real receivable via the money engine, then exercise the public
			// invoice administration workflow entirely through the common Client.
			_, err = rt.MoneyService.AccrueOwed(scoped, payer, "USD", "invoice-client", uuid.NewString(), 500)
			require.NoError(t, err)
			invoice, err := rt.MoneyService.FinalizeInvoice(scoped, payer, "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
			require.NoError(t, err)
			pid := payer.UUID()
			listed, total, err := client.ListMerchantInvoices(ctx, openrails.MerchantInvoiceFilter{CustomerID: &pid}, 10, 0)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, listed, 1)
			require.Equal(t, invoice.ID, listed[0].ID)
			require.EqualValues(t, 500, listed[0].AmountDue)
			read, err := client.GetMerchantInvoice(ctx, invoice.ID)
			require.NoError(t, err)
			require.Equal(t, invoice.ID, read.ID)
			require.Contains(t, read.AvailableActions, openrails.InvoiceAdminRecordPayment)
			paid, err := client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 200, Reference: "bank-transfer"})
			require.NoError(t, err)
			require.EqualValues(t, 300, paid.AmountDue)
			_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 200, Reference: "bank-transfer"})
			require.ErrorIs(t, err, openrails.ErrConflict)
			attempts, _, err := client.ListInvoicePaymentAttempts(ctx, invoice.ID, 10, 0)
			require.NoError(t, err)
			require.NotNil(t, attempts)
			voided, err := client.VoidInvoice(ctx, invoice.ID)
			require.NoError(t, err)
			require.Equal(t, "voided", voided.Status)
			_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 1, Reference: "after-void"})
			require.ErrorIs(t, err, openrails.ErrConflict)
			_, err = client.GetMerchantInvoice(ctx, uuid.New())
			require.ErrorIs(t, err, openrails.ErrNotFound)
		})
	}
}
