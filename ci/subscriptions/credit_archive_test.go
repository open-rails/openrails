//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/stretchr/testify/require"
)

func TestPurchasedCreditArchivePreservesDepositsAndReversals(t *testing.T) {
	for _, rail := range []string{"nmi", "stripe"} {
		t.Run(rail, func(t *testing.T) { purchasedCreditArchive(t, rail) })
	}
}

func purchasedCreditArchive(t *testing.T, rail string) {
	w := newWorld(t)
	c := w.client[embedded]
	buyer := w.newCustomer()
	method := buyer.saveCard(rail, visa)
	deposit, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "archive-deposit", DisplayName: "Deposit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	selected, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: deposit.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	bonus, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "archive-bonus", DisplayName: "Bonus pack", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: new(int64(120_000_000))}})
	require.NoError(t, err)
	fixed, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: bonus.ID, Key: "pack", Currency: "USD", UnitAmount: 100_000_000})
	require.NoError(t, err)
	buy := func(price billing.PriceID, amount *int64) *billing.CheckoutAttempt {
		t.Helper()
		paid, err := c.CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptParams{Customer: buyer.identity(), PriceID: price, Amount: amount, IdempotencyKey: price.String(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)}})
		require.NoError(t, err)
		require.Equal(t, billing.CheckoutAttemptSucceeded, paid.Status)
		return paid
	}
	buy(selected.ID, new(int64(100_000_000)))
	paidBonus := buy(fixed.ID, nil)
	_, err = c.RefundPayment(t.Context(), *paidBonus.PaymentID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "archive-bonus-refund"})
	require.NoError(t, err)
	w.settle()
	grants, err := c.ListCreditGrants(t.Context(), buyer.cid(), billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, grants.Items, 2)
	var available int64
	for _, grant := range grants.Items {
		available += grant.RemainingAmount
	}
	require.Equal(t, int64(100_000_000), available, "only the unrefunded deposit remains spendable")
	events, err := c.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err = c.AcknowledgeHostEvent(t.Context(), event.ID)
		require.NoError(t, err)
	}
	merchantID := c.MerchantID()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var artifact bytes.Buffer
	err = merchantarchive.Export(t.Context(), source, merchantID, &artifact)
	require.NoError(t, err, "archive cause: %v", errors.Unwrap(err))
	schema := "archive_credit_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: schema, RiverSchema: schema}))
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+`.merchants (id,slug,status,permission_group_id,display_name) VALUES ($1,$2,'active',$3,'Restored')`, merchantID.UUID(), w.slug, uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	// An intact artifact with a narrower deposit range contradicts the accepted
	// purchase. Restore rejects it atomically; the valid artifact can follow.
	var changed bytes.Buffer
	var writer *archivewire.Writer
	table := ""
	_, err = archivewire.Read(bytes.NewReader(artifact.Bytes()), func(header archivewire.Header) error {
		var err error
		writer, err = archivewire.NewWriter(&changed, header.MerchantID, header.CatalogRevision)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			return writer.Table(table)
		}
		if table == "prices" && record.Values[2] != nil && *record.Values[2] == selected.ID.UUID().String() {
			record.Values[len(record.Values)-1] = new(`{"min_amount":"1000000","max_amount":"2000000"}`)
		}
		return writer.Row(record.Values)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, err = merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(changed.Bytes()))
	require.Error(t, err, "altered bounds cannot reinterpret the original accepted amount")
	result, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err, "restore cause: %v", errors.Unwrap(err))
	require.Positive(t, result.Rows)
	replay, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	for _, table := range []string{"products", "prices", "payments", "grants", "ledger_accounts", "ledger_transfers"} {
		var before, after string
		sql := fmt.Sprintf("SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb)::text FROM %%s.%s t WHERE merchant_id=$1", pgx.Identifier{table}.Sanitize())
		require.NoError(t, w.pool.QueryRow(t.Context(), fmt.Sprintf(sql, pgx.Identifier{w.schema}.Sanitize()), merchantID.UUID()).Scan(&before))
		require.NoError(t, w.pool.QueryRow(t.Context(), fmt.Sprintf(sql, quoted), merchantID.UUID()).Scan(&after))
		require.JSONEq(t, before, after, table+" preserves exact IDs, native terms, source lot, dates and money facts")
	}
}
