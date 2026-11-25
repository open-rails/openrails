package merchantbootstrap

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
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/stripemock"
)

// A one-off tool's load of a merchant registers its Stripe PSPs' posture as
// a server's does, so the tool's write is sent on its PSP's check rather than
// refused for want of one.
func TestAOneOffToolsStripeWriteIsSentOnItsPSPsCheck(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	svc := &merchants.Service{StripeClients: stripeapi.NewWiredFactory(stripe, controls.Fixed(controls.Full))}
	id := billing.MerchantID(uuid.New())
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name
	loaded := merchantdocs.Set{PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{"stripe": {Value: merchantdocs.PSP{
		Rail: "stripe", Environment: "test", AccountID: "acct_" + name, Secrets: map[string]string{"secret_key": key}}}}}
	_, err := oneOffSync(svc)(context.Background(), id, merchantdocs.Set{}, loaded)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(merchant.WithID(context.Background(), id), http.MethodPost, stripeapi.APIBase+"/v1/refunds", strings.NewReader("payment_intent=pi_tool&amount=100"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := svc.StripeClients.Client(0).Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Len(t, stripe.Submitted("/v1/refunds"), 1)
}
