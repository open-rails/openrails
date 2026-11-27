//go:build integration

package critical_test

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// An upgrade keeps every invoice an earlier build wrote: a merchant invoice
// loses the expiry it was given, a customer's purchase keeps its own.
func TestInvoiceExpiryMigration(t *testing.T) {
	t.Run("OR-J08-S75/an-upgrade-clears-every-merchant-invoices-expiry-and-keeps-a-purchases", merchantExpiriesCleared)
}

func merchantExpiriesCleared(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := criticalSchema()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	logger := slog.New(slog.DiscardHandler)
	// The chain an earlier build shipped: everything before merchant invoices
	// stopped expiring.
	earlier := fstest.MapFS{}
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	for _, name := range names {
		if name >= "0012_" {
			continue
		}
		data, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		earlier[name] = &fstest.MapFile{Data: data}
	}
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: earlier, Logger: logger}))

	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	merchant, customer := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, "INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgraded', 'active')", merchant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO "+table("customers")+" (merchant_id, id) VALUES ($1, $2)", merchant, customer)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(columns, values string, args ...any) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, "INSERT INTO "+table("invoices")+
			" (merchant_id, customer_id, currency, "+columns+") VALUES ($1, $2, 'USD', "+values+") RETURNING id",
			append([]any{merchant, customer}, args...)...).Scan(&id))
		return id
	}
	// An open bill with the 30 days an earlier build gave it.
	open := insert("origin, status, payment_status, total, expires_at", "'merchant', 'open', 'requires_payment_method', 9990000, $3", now.Add(30*24*time.Hour))
	// One with a pay URL, which lasted no longer than the invoice.
	payURL := now.Add(time.Hour)
	linked := insert("origin, status, payment_status, total, expires_at, pay_url_secret_hash, pay_url_expires_at, success_url, saved_payment_methods, embedded",
		"'merchant', 'open', 'requires_payment_method', 9990000, $3, $4, $3, 'https://alpha.critical.invalid/done', false, false", payURL, make([]byte, 32))
	// An imported purchase, settled as it was recorded with a day's expiry.
	imported := insert("origin, status, payment_status, total, expires_at, number, completed_at", "'merchant', 'complete', 'succeeded', 0, $3, 1001, $4", now.Add(24*time.Hour), now)
	// A customer's own purchase keeps its expiry.
	purchase := insert("origin, status, payment_status, total, expires_at", "'customer', 'open', 'requires_payment_method', 9990000, $3", now.Add(24*time.Hour))

	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}), "an upgrade over merchant invoices that had an expiry")
	expiry := func(id uuid.UUID) *time.Time {
		t.Helper()
		var at *time.Time
		require.NoError(t, pool.QueryRow(ctx, "SELECT expires_at FROM "+table("invoices")+" WHERE merchant_id = $1 AND id = $2", merchant, id).Scan(&at))
		return at
	}
	for _, id := range []uuid.UUID{open, linked, imported} {
		require.Nil(t, expiry(id), "a merchant invoice has no expiry")
	}
	var linkExpires time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT pay_url_expires_at FROM "+table("invoices")+" WHERE merchant_id = $1 AND id = $2", merchant, linked).Scan(&linkExpires))
	require.True(t, payURL.Equal(linkExpires), "its pay URL keeps its own expiry")
	kept := expiry(purchase)
	require.NotNil(t, kept)
	require.True(t, now.Add(24*time.Hour).Equal(*kept), "a purchase keeps its expiry")
}
