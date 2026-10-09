//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/writeposture"
)

// A database the billing book was not armed in is a copy of it (a dump
// restored elsewhere, another schema): it charges nothing, renewal or
// purchase, until an operator arms it.
func TestCopiedDatabaseIsReadonlyUntilArmed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	buyer := w.newCustomer()
	card := buyer.saveCard("nmi", visa)
	// The book names the database it was dumped from.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.book_identity SET system_identifier = '0'`))
	require.NoError(t, err)

	e.toFreshPeriodEnd()
	w.runRenewals()
	w.settle()
	require.Len(t, e.providerLedger(), 1, "a copy charges no renewal")
	require.Equal(t, end, e.periodEnd())
	paid, err := buyer.checkout(embedded, order{price: pid(e.price), rail: "nmi", method: card, successURL: "https://e2e.test/return"})
	require.True(t, err != nil || paid.Status != "succeeded", "a copy sells nothing")
	require.Len(t, e.providerLedger(), 1, "nothing is charged")

	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, writeposture.ArmBook(t.Context(), database.GenDirectory(), "e2e cutover", w.clock.Now()))
	w.runRenewals()
	w.until(func() bool { return e.periodEnd().After(end) }, "the armed book renews")
	require.Len(t, e.providerLedger(), 2, "one renewal once armed")
}
