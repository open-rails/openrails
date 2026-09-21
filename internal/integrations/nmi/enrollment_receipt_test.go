package nmi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestEnrollmentEvidenceRequiresOneCorrelatedSchedule(t *testing.T) {
	const record = `<subscription id="123"><subscription_id>123</subscription_id><plan><plan_id>plan</plan_id></plan><orderid>operation-successor</orderid><ponumber>operation-successor</ponumber><next_charge_date>2026-10-21</next_charge_date></subscription>`
	for _, tc := range []struct {
		name, record string
		valid        bool
	}{
		{"exact schedule", record, true},
		{"absent", "", false},
		{"ambiguous", record + record, false},
		{"wrong identity", strings.ReplaceAll(record, "123", "456"), false},
		{"contradictory identity", strings.Replace(record, `id="123"`, `id="456"`, 1), false},
		{"wrong plan", strings.ReplaceAll(record, "<plan_id>plan</plan_id>", "<plan_id>other</plan_id>"), false},
		{"missing order", strings.Replace(record, "<orderid>operation-successor</orderid>", "", 1), false},
		{"malformed", "<subscription>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					require.Equal(t, "/subscriptions/123", r.URL.Path)
					fmt.Fprint(w, `{"id":"123","customer_vault_id":"vault","delayed_condition":"active","plan":{"id":"plan"}}`)
				case http.MethodPost:
					require.NoError(t, r.ParseForm())
					require.Equal(t, "recurring", r.Form.Get("report_type"))
					require.Equal(t, "123", r.Form.Get("subscription_id"))
					reads++
					fmt.Fprint(w, "<nm_response>"+tc.record+"</nm_response>")
				default:
					t.Errorf("unexpected provider mutation %s", r.Method)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "synthetic-enrollment-key", WebhookSecret: "synthetic-webhook-key"}, true)
			require.NoError(t, err)
			client.V5BaseURL, client.QueryURL = server.URL, server.URL
			facts, found, err := client.ReadEnrollmentEvidence(t.Context(), "123")
			require.Equal(t, 1, reads)
			if !tc.valid {
				require.ErrorIs(t, err, ErrReceiptMismatch)
				require.False(t, found)
				return
			}
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "operation-successor", facts.OrderReference)
			require.Equal(t, facts.OrderReference, facts.PONumber)
			require.Equal(t, "2026-10-21", facts.NextChargeDate)
			require.Equal(t, "vault", facts.Subscription.CustomerVaultID)
		})
	}
}
