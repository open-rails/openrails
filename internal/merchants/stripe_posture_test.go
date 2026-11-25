package merchants

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/stripemock"
)

// stripeLoad is one process loading a merchant's Stripe PSPs over the Stripe
// fixture, gated as against Stripe.
type stripeLoad struct {
	stripe   *stripemock.Mock
	factory  *stripeapi.Factory
	merchant billing.MerchantID
	name     string // the fixture's account is acct_<name> for this key
	key      string
}

func newStripeLoad() *stripeLoad {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	return &stripeLoad{stripe: stripe, factory: stripeapi.NewWiredFactory(stripe, controls.Fixed(controls.Full)), merchant: billing.MerchantID(uuid.New()), name: name, key: "sk_test_acct_" + name}
}

func (l *stripeLoad) set(psps map[string]merchantdocs.PSP) merchantdocs.Set {
	out := merchantdocs.Set{PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{}}
	for key, p := range psps {
		out.PSPs[key] = merchantdocs.Doc[merchantdocs.PSP]{Value: p}
	}
	return out
}

func (l *stripeLoad) psp(account string, archived bool) merchantdocs.PSP {
	return merchantdocs.PSP{Rail: "stripe", Environment: "test", AccountID: account, Archived: archived, Secrets: map[string]string{"secret_key": l.key}}
}

// refund is a refund the merchant's key writes; nil when Stripe got it.
func (l *stripeLoad) refund(t *testing.T) error {
	t.Helper()
	req, err := http.NewRequestWithContext(merchant.WithID(context.Background(), l.merchant), http.MethodPost, stripeapi.APIBase+"/v1/refunds", strings.NewReader("payment_intent=pi_posture&amount=100"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+l.key)
	response, err := l.factory.Client(0).Do(req)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

// After a restart no key has a verdict, and an archived PSP is no live PSP;
// the load of its merchant's configuration registers it, so its refund goes
// out on its own check's verdict.
func TestAnArchivedStripePSPsRefundAfterARestartIsSent(t *testing.T) {
	l := newStripeLoad()
	RegisterStripePosture(l.factory, l.merchant, merchantdocs.Set{}, l.set(map[string]merchantdocs.PSP{"retired": l.psp("acct_"+l.name, true)}))
	require.NoError(t, l.refund(t))
	require.Len(t, l.stripe.Submitted("/v1/refunds"), 1)
}

// Correcting a typo'd account_id replaces the PSP's registration: its key
// writes again without a restart.
func TestACorrectedStripeAccountArmsWithoutARestart(t *testing.T) {
	l := newStripeLoad()
	typo := l.set(map[string]merchantdocs.PSP{"stripe": l.psp("acct_typo"+l.name, false)})
	RegisterStripePosture(l.factory, l.merchant, merchantdocs.Set{}, typo)
	err := l.refund(t)
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.ErrorContains(t, err, "mismatched")
	RegisterStripePosture(l.factory, l.merchant, typo, l.set(map[string]merchantdocs.PSP{"stripe": l.psp("acct_"+l.name, false)}))
	require.NoError(t, l.refund(t))
	require.Len(t, l.stripe.Submitted("/v1/refunds"), 1)
}

// A duplicate document declaring the original's key with a wrong account
// disarms only itself; once the original is gone, nothing proves the key.
func TestAWrongDuplicateDoesNotDisarmTheOriginal(t *testing.T) {
	l := newStripeLoad()
	both := l.set(map[string]merchantdocs.PSP{"stripe": l.psp("acct_"+l.name, false), "copy": l.psp("acct_wrong"+l.name, false)})
	RegisterStripePosture(l.factory, l.merchant, merchantdocs.Set{}, both)
	require.NoError(t, l.refund(t))
	RegisterStripePosture(l.factory, l.merchant, both, l.set(map[string]merchantdocs.PSP{"copy": l.psp("acct_wrong"+l.name, false)}))
	require.ErrorIs(t, l.refund(t), providerposture.ErrDisarmed)
	require.Len(t, l.stripe.Submitted("/v1/refunds"), 1)
}
