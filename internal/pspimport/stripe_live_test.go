package pspimport

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/reconcile"
)

// The Stripe pull reads a real test-mode account: set
// OPENRAILS_TEST_STRIPE_SECRET_KEY to a test-mode key (sk_test_ or rk_test_).
// Its client blocks every write before the network.
func TestStripePullReadsATestAccount(t *testing.T) {
	key := os.Getenv("OPENRAILS_TEST_STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("OPENRAILS_TEST_STRIPE_SECRET_KEY names no test account")
	}
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("only a test-mode key is read")
	}
	src := &stripeSource{fetcher: reconcile.NewStripeFetcher(key)}
	roster, err := src.roster(t.Context())
	require.NoError(t, err)
	kinds := map[billing.PSPImportRecordKind]int{}
	for _, r := range roster {
		kinds[r.kind]++
		require.NotEmpty(t, r.ref)
		if r.kind == billing.PSPImportSubscription {
			require.NotEmpty(t, r.customerRef)
			require.False(t, r.data.(subscriptionData).StartedAt.IsZero())
		}
	}
	until := time.Now().UTC()
	history, err := src.history(t.Context(), until.AddDate(0, 0, -30), until)
	require.NoError(t, err)
	attributed := 0
	for _, h := range history {
		if h.subscriptionRef != "" {
			attributed++
		}
	}
	t.Logf("staged %d customers, %d subscriptions; %d transactions in 30 days, %d on a subscription", kinds[billing.PSPImportCustomer], kinds[billing.PSPImportSubscription], len(history), attributed)
}
