//go:build e2e && integration

package subscriptions_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The ordinary insert-only pull can read the same gateway's full history while
// an exact operation receipt is temporarily unavailable. A shared vault is not
// authority to settle its invoice or renewal through another writer.
func TestNMIPullDoesNotReplaceCanonicalCollection(t *testing.T) {
	t.Parallel()
	for _, invoiceMode := range []bool{false, true} {
		name := "renewal"
		if invoiceMode {
			name = "invoice"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", remote)
			end := e.periodEnd()
			if !invoiceMode {
				e.refreshBeforePeriodEnd()
			}
			w.nmi.Intercept(func(r *http.Request) bool {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/query.php") {
					return false
				}
				form, err := url.ParseQuery(readBody(r))
				return err == nil && form.Get("report_type") == "transaction" && form.Get("order_id") != "" && len(w.nmi.ledger("")) > 1
			}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("temporarily unavailable")), Request: r}, nil
			})
			var invoice billing.InvoiceID
			if invoiceMode {
				_, err := w.client[remote].SetCreditLimit(t.Context(), e.c.cid(), billing.SetCreditLimitParams{Currency: "USD", Amount: 50_000_000})
				require.NoError(t, err)
				_, err = recordUsage(t.Context(), w.client[remote], billing.RecordUsageParams{CustomerID: e.c.cid(), Invoker: e.c.id, Currency: "USD", EventType: "pull-invoice", Amount: 50_000_000, Source: "test", SourceID: uuid.NewString()})
				require.NoError(t, err)
				w.advance(time.Minute)
				job, err := w.jobs.Insert(t.Context(), monthlyInvoicePass{FinalizePreviousMonth: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				w.waitJob(job.Job.ID)
				list, err := w.client[remote].ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: e.c.cid()})
				require.NoError(t, err)
				for _, candidate := range list.Items {
					requested := false
					for _, line := range candidate.LineItems {
						requested = requested || line.EventType == "pull-invoice" && line.Amount == 50_000_000 && line.Count == 1
					}
					if requested {
						require.True(t, invoice.IsZero(), "one statement contains the requested usage")
						require.Equal(t, "USD", candidate.Currency)
						require.Equal(t, int64(50_000_000), candidate.TotalAmount)
						require.Equal(t, candidate.TotalAmount, candidate.AmountDue)
						invoice = candidate.ID
					} else {
						require.Zero(t, candidate.TotalAmount, "other monthly statements carry no charge")
						require.Zero(t, candidate.AmountPaid)
						require.Zero(t, candidate.AmountDue)
						require.Equal(t, billing.InvoicePaid, candidate.Status)
					}
				}
				require.False(t, invoice.IsZero(), "the requested usage was invoiced")
				w.refreshProviders()
				w.settleCollectionScans()
				status, body := e.c.call(http.MethodPost, "/invoices/"+invoice.String()+"/pay-now", "pull-invoice", map[string]string{"payment_method_id": e.method})
				require.Equal(t, http.StatusAccepted, status, body)
			} else {
				e.toPeriodEnd()
				w.runRenewals()
			}
			require.Len(t, w.nmi.ledger(""), 2, "the provider has already collected the operation")
			require.Len(t, completed(w.payments(remote, e.c.id)), 1, "only signup is settled locally")
			transaction := w.nmi.ledger("")[1].ID
			insertPull := func() {
				var out strings.Builder
				err := hosttools.PullProvider(t.Context(), hosttools.PullProviderOptions{
					Config:  &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema}},
					PGXPool: w.pool, MerchantID: w.client[embedded].MerchantID(),
					MerchantManifest: &merchantbootstrap.BillingConfig{Merchants: map[string]openrails.MerchantDeclaration{w.slug: {DisplayName: w.slug, PSPs: w.psps}}},
					NMITransport:     w.nmi, Providers: []string{"nmi"}, Insert: true,
					Since: w.clock.Now().Add(-90 * day).UTC().Format(time.RFC3339), Until: w.clock.Now().Add(time.Second).UTC().Format(time.RFC3339), Out: &out,
				})
				require.NoError(t, err, out.String())
			}
			insertPull()
			require.Len(t, completed(w.payments(remote, e.c.id)), 1, "generic pull cannot turn an invoice/native renewal into a subscription payment")
			require.True(t, e.periodEnd().Equal(end), "only accepted collection settlement advances the subscription")
			var finding string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.reconciliation_findings WHERE finding_type='pull.charge.missing' AND subject_key=$1`), transaction).Scan(&finding))
			require.Equal(t, "requires_review", finding)
			if invoiceMode {
				inv, err := w.client[remote].GetInvoice(t.Context(), invoice)
				require.NoError(t, err)
				require.Equal(t, int64(50_000_000), inv.AmountDue)
			}
			w.nmi.ClearIntercepts()
			w.advance(time.Hour)
			w.wake()
			w.settle()
			if invoiceMode {
				inv, err := w.client[remote].GetInvoice(t.Context(), invoice)
				require.NoError(t, err)
				require.Equal(t, billing.InvoicePaid, inv.Status)
				require.True(t, e.periodEnd().Equal(end))
				require.Len(t, completed(w.payments(remote, e.c.id)), 1)
			} else {
				require.True(t, e.periodEnd().Equal(end.Add(monthHours*time.Hour)))
				require.Len(t, completed(w.payments(remote, e.c.id)), 2)
			}
			// Exact canonical invoice receipts are recognized outside billing.payments;
			// the next pull neither manufactures another payment nor leaves false debt.
			insertPull()
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.reconciliation_findings WHERE finding_type='pull.charge.missing' AND subject_key=$1`), transaction).Scan(&finding))
			require.Contains(t, []string{"fixed", "auto_fixed"}, finding)
			require.Len(t, w.nmi.ledger(""), 2)
			require.Len(t, w.nmi.Attempts(), 2, "reconciliation made no additional provider charge")
		})
	}
}
