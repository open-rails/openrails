//go:build integration

package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

func TestSelfInvoicesHTTP_ReflectsReceivablePaymentsAndScopesToSubject(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	subjectA := uuid.NewString()
	subjectB := uuid.NewString()
	payerA := identity.CustomerIDFromString(subjectA)
	customerA := suite.ensureCustomer(ctx, subjectA)

	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", customerA)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", customerA)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", customerA)
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", customerA)
	})

	ms := suite.App.Runtime.MoneyService
	_, err := ms.UpsertAccountSettings(ctx, payerA, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: stringPtr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	_, err = ms.AccrueOwed(ctx, payerA, money.DefaultCurrency, "usage", "self-http-invoice-"+uuid.NewString(), 500)
	require.NoError(t, err)

	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	inv, err := ms.FinalizeInvoice(ctx, payerA, money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.EqualValues(t, 500, inv.AmountDue)

	partial, err := ms.RecordOutOfBandInvoicePayment(ctx, payerA, inv.ID, 200, "self-http-manual-1-"+uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, "open", partial.Status)
	require.EqualValues(t, 200, partial.AmountPaid)
	require.EqualValues(t, 300, partial.AmountDue)

	routerA := newHostSeamSelfRouter(t, suite, subjectA, []string{controlplane.PermSelfBillingRead})
	routerB := newHostSeamSelfRouter(t, suite, subjectB, []string{controlplane.PermSelfBillingRead})

	w := doHostSeamSelf(routerA, http.MethodGet, "/v1/me/invoices?limit=10", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	listBody := decodeHostSeamBody(t, w)
	require.EqualValues(t, 1, listBody["total"], w.Body.String())
	rows, ok := listBody["data"].([]any)
	require.True(t, ok, w.Body.String())
	require.Len(t, rows, 1)
	listed, ok := rows[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, inv.ID.String(), listed["id"])
	require.Equal(t, "open", listed["status"])
	require.EqualValues(t, 500, listed["total_amount"])
	require.EqualValues(t, 200, listed["amount_paid"])
	require.EqualValues(t, 300, listed["amount_due"])

	w = doHostSeamSelf(routerA, http.MethodGet, "/v1/me/invoices/"+inv.ID.String(), "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	detail := decodeHostSeamBody(t, w)
	require.Equal(t, inv.ID.String(), detail["id"])
	require.Equal(t, "open", detail["status"])
	require.EqualValues(t, 300, detail["amount_due"])

	paid, err := ms.RecordOutOfBandInvoicePayment(ctx, payerA, inv.ID, 300, "self-http-manual-2-"+uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.EqualValues(t, 0, paid.AmountDue)

	w = doHostSeamSelf(routerA, http.MethodGet, "/v1/me/invoices/"+inv.ID.String(), "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	paidDetail := decodeHostSeamBody(t, w)
	require.Equal(t, "paid", paidDetail["status"])
	require.EqualValues(t, 500, paidDetail["amount_paid"])
	require.EqualValues(t, 0, paidDetail["amount_due"])
	require.NotEmpty(t, paidDetail["paid_at"])

	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/invoices?limit=10", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	subjectBList := decodeHostSeamBody(t, w)
	require.EqualValues(t, 0, subjectBList["total"], "subject B must not see subject A invoices: %s", w.Body.String())

	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/invoices/"+inv.ID.String(), "")
	require.Equal(t, http.StatusNotFound, w.Code, "subject B must not read subject A invoice detail: %s", w.Body.String())
}

func stringPtr(s string) *string { return &s }
