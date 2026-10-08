//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type enrichmentPass struct{}

func (enrichmentPass) Kind() string { return "openrails.attempt_enrichment" }

// enrichAttempts runs the NMI attempt enrichment pass once (#1114).
func (w *world) enrichAttempts() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), enrichmentPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// enriched is one attempt's card and issuer columns (#1114).
type enriched struct {
	Kind                                          string
	BIN, Brand, Last4, AVS, IssuerCode, TokenType *string
	EnrichedAt                                    *time.Time
}

func (w *world) enrichedAttempts(customerID string) []enriched {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT kind, card_bin, card_brand, card_last4, avs_result, issuer_code, token_type, enriched_at
		FROM `+pgx.Identifier{w.schema}.Sanitize()+`.payment_attempts WHERE customer_id = $1 ORDER BY attempted_at, id`, customerID)
	require.NoError(w.t, err)
	defer rows.Close()
	var out []enriched
	for rows.Next() {
		var e enriched
		require.NoError(w.t, rows.Scan(&e.Kind, &e.BIN, &e.Brand, &e.Last4, &e.AVS, &e.IssuerCode, &e.TokenType, &e.EnrichedAt))
		out = append(out, e)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// NMI's transaction report fills every NMI attempt once: the card's BIN and
// brand, AVS, and the issuer's raw answer (#1114).
func TestNMIAttemptEnrichment(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	card := visa
	card.AVS = "Y"
	c := w.newCustomer()
	c.saveCard("nmi", card)
	e := enroll(t, w, "nmi", embedded)
	w.advance(time.Hour)
	w.enrichAttempts() // the pass reads the last 30 days
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	w.runRenewals()
	w.advance(time.Hour)
	w.enrichAttempts()

	saved := w.enrichedAttempts(c.id)
	require.Len(t, saved, 1)
	require.Equal(t, []string{"verify", "Y", "00"}, []string{saved[0].Kind, str(saved[0].AVS), str(saved[0].IssuerCode)})
	rows := w.enrichedAttempts(e.c.id)
	require.GreaterOrEqual(t, len(rows), 3, "the verification, the initial charge and the rebill")
	for _, r := range rows {
		require.NotNil(t, r.EnrichedAt, r.Kind)
		require.Equal(t, []string{"411111", "visa", visa.Last4, "psp_token"}, []string{str(r.BIN), str(r.Brand), str(r.Last4), str(r.TokenType)}, r.Kind)
	}
	rebill := rows[len(rows)-1]
	require.Equal(t, []string{"rebill", "51"}, []string{rebill.Kind, str(rebill.IssuerCode)}, "the issuer's own code behind NMI's 202")

	w.advance(time.Hour)
	w.enrichAttempts()
	require.Equal(t, rows, w.enrichedAttempts(e.c.id), "a rerun changes nothing")
}

// A card NMI charges through a network token is recorded as one (#1114).
func TestNMIAttemptNetworkToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	card := visa
	card.NetworkToken = true
	c := w.newCustomer()
	c.saveCard("nmi", card)
	w.advance(time.Hour)
	w.enrichAttempts()
	rows := w.enrichedAttempts(c.id)
	require.Len(t, rows, 1)
	require.Equal(t, "network_token", str(rows[0].TokenType))
}
