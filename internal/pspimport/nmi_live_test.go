package pspimport

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// readOnlyWire fails any NMI request that is not a read: a v5 GET or a
// Query API report.
type readOnlyWire struct{ t *testing.T }

func (w readOnlyWire) RoundTrip(req *http.Request) (*http.Response, error) {
	if !(req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/api/v5/")) && !(req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/api/query.php")) {
		w.t.Fatalf("the pull sent %s %s", req.Method, req.URL.Path)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// The NMI pull reads a real test-mode gateway: set
// OPENRAILS_TEST_NMI_SECURITY_KEY to a test-mode gateway's key. It sends only
// reads and stages every object with its identity facts.
func TestNMIPullReadsATestGateway(t *testing.T) {
	key := os.Getenv("OPENRAILS_TEST_NMI_SECURITY_KEY")
	if key == "" {
		t.Skip("OPENRAILS_TEST_NMI_SECURITY_KEY names no test gateway")
	}
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi-test", &config.NMIProviderSettings{SecurityKey: key, Environment: config.EnvironmentTest})
	require.NoError(t, err)
	client.ReadOnly = true
	client.UseTransport(readOnlyWire{t})
	src := &nmiSource{client: client, currency: "USD"}

	roster, err := src.roster(t.Context())
	require.NoError(t, err)
	kinds, withOrder, withEmail, withShipping, withCards := map[billing.PSPImportRecordKind]int{}, 0, 0, 0, 0
	attrib := newAttribution()
	for _, r := range roster {
		kinds[r.kind]++
		require.NotEmpty(t, r.ref)
		switch r.kind {
		case billing.PSPImportCustomer:
			require.Equal(t, []string{r.ref}, r.facts[FactCustomerRef])
			if len(r.facts[FactShippingID]) > 0 {
				withShipping++
			}
			if len(r.data.(customerData).Cards) > 0 {
				withCards++
			}
		case billing.PSPImportSubscription:
			data := r.data.(subscriptionData)
			require.NotEmpty(t, r.customerRef, "a v5 subscription names its vault")
			require.False(t, data.StartedAt.IsZero())
			require.Positive(t, data.Amount)
			if len(r.facts[FactOrderID]) > 0 {
				withOrder++
			}
			attrib.addSubscription(r.ref, r.customerRef, r.facts, data.StartedAt)
		}
		if len(r.facts[FactEmail]) > 0 {
			withEmail++
		}
	}
	require.Positive(t, kinds[billing.PSPImportCustomer])
	require.Positive(t, withCards)
	if kinds[billing.PSPImportSubscription] > 0 {
		require.Positive(t, withOrder, "the recurring report joins order ids")
	}

	until := time.Now().UTC()
	history, err := src.history(t.Context(), until.AddDate(0, 0, -30), until)
	require.NoError(t, err)
	attributed, vaulted := 0, 0
	for i := range history {
		attrib.place(&history[i])
		if history[i].subscriptionRef != "" {
			attributed++
		}
		if history[i].customerRef != "" {
			vaulted++
		}
		data := history[i].data.(transactionData)
		require.NotEmpty(t, data.TransactionID)
		require.Contains(t, []string{"sale", "decline", "refund"}, data.Type)
	}
	t.Logf("staged %d customers (%d with a shipping id, %d with cards), %d subscriptions (%d with an order id), %d with an email; %d transactions in 30 days, %d naming a vault, %d attributed",
		kinds[billing.PSPImportCustomer], withShipping, withCards, kinds[billing.PSPImportSubscription], withOrder, withEmail, len(history), vaulted, attributed)
}
