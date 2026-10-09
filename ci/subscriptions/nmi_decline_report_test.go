//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
)

// declineReport runs `openrails nmi decline-report` against this world.
func (w *world) declineReport(since time.Time, format string) string {
	w.t.Helper()
	var out strings.Builder
	err := hosttools.NMIDeclineReport(w.t.Context(), hosttools.NMIDeclineReportOptions{
		Config:           &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema}},
		PGXPool:          w.pool,
		MerchantID:       w.client[embedded].MerchantID(),
		MerchantManifest: &merchantbootstrap.BillingConfig{Merchants: map[string]openrails.MerchantDeclaration{w.slug: {DisplayName: w.slug, PSPs: w.psps}}},
		NMITransport:     http.RoundTripper(w.nmi),
		Since:            since.UTC().Format(time.RFC3339),
		Until:            w.clock.Now().UTC().Format(time.RFC3339),
		Format:           format,
		Out:              &out,
	})
	require.NoError(w.t, err)
	return out.String()
}

func (w *world) rows(table string) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FROM billing.`+table)).Scan(&n))
	return n
}

// The baseline report reads an NMI account's history and writes nothing: a
// verification, a one-off sale, and NMI's scheduled rebills, one refused
// for insufficient funds (#1114).
func TestNMIDeclineReport(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	since := w.clock.Now().Add(-30 * day)
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor) // its signup sale is a one-off sale
	w.converge()
	w.newCustomer().saveCard("nmi", card{Brand: "mastercard", Last4: "4444"})
	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true))) // money NMI moved is recorded
	w.settle()
	w.nmi.SetDecline(visa.Last4, "202")
	w.nmi.RenewSchedule(l.railSub, false) // refused next period, seen by nobody
	w.advance(31 * day)
	attempts, payments := w.rows("payment_attempts"), w.rows("payments")

	var report struct {
		Months []struct {
			Kind                        string
			Attempts, Approved, Refused int
		}
		Refusals []struct {
			Kind, Category, Reason string
			Count                  int
		}
		Notes []string
	}
	require.NoError(t, json.Unmarshal([]byte(w.declineReport(since, "json")), &report))
	totals := map[string][3]int{}
	for _, m := range report.Months {
		c := totals[m.Kind]
		totals[m.Kind] = [3]int{c[0] + m.Attempts, c[1] + m.Approved, c[2] + m.Refused}
	}
	require.Equal(t, map[string][3]int{"verification": {1, 1, 0}, "one_off_sale": {1, 1, 0}, "scheduled_rebill": {2, 1, 1}}, totals)
	require.Len(t, report.Refusals, 1)
	require.Equal(t, []string{"scheduled_rebill", "issuer_soft", "insufficient_funds"}, []string{report.Refusals[0].Kind, report.Refusals[0].Category, report.Refusals[0].Reason})
	require.NotEmpty(t, report.Notes)

	require.Contains(t, w.declineReport(since, "table"), "scheduled_rebill")
	require.Equal(t, attempts, w.rows("payment_attempts"), "the report writes nothing")
	require.Equal(t, payments, w.rows("payments"))
}
