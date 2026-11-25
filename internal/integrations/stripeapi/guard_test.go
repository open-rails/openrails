package stripeapi_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/stripemock"
)

type wireFunc func(*http.Request) (*http.Response, error)

func (f wireFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func send(t *testing.T, client *http.Client, method, key string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, stripeapi.APIBase+"/v1/customers", strings.NewReader("email=a%40example.com"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := client.Do(req)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

// A key no loaded PSP declares writes nothing; once its PSP registers it, a
// write waits for that first check and goes out on its verdict. Reads are
// never held.
func TestAWriteIsSentOnlyOnItsPSPsVerdict(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	factory := stripeapi.NewWiredFactory(stripe, controls.Fixed(controls.Full))
	client := factory.Client(0)
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name

	err := send(t, client, http.MethodPost, key)
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.ErrorIs(t, err, providerposture.ErrPending)
	require.NotContains(t, err.Error(), key)
	require.Empty(t, stripe.Submitted("/v1/customers"), "a write no PSP registered is not sent")
	require.NoError(t, send(t, client, http.MethodGet, key))

	factory.RegisterPosture("guard/"+name, uuid.New(), uuid.New(), "acct_"+name, key)
	require.NoError(t, send(t, client, http.MethodPost, key))
	require.Len(t, stripe.Submitted("/v1/customers"), 1)
	require.Equal(t, 1, stripe.Reads("/v1/account"), "the registered check ran once")
}

// A key Stripe stops accepting (401 on a write) is refused from then on before
// a byte is sent: the write did nothing, and so does every next one until a
// re-check or a new key arms it.
func TestARevokedKeyStopsItsWrites(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	factory := stripeapi.NewWiredFactory(stripe, controls.Fixed(controls.Full))
	client := factory.Client(0)
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name
	factory.RegisterPosture("revoked/"+name, uuid.New(), uuid.New(), "acct_"+name, key)
	stripe.Intercept(func(r *http.Request) bool { return r.Method == http.MethodPost }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Expired API Key provided: ` + key + `"}}`))}, nil
	})
	err := send(t, client, http.MethodPost, key)
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.ErrorContains(t, err, "401")
	require.NotContains(t, err.Error(), key)
	require.ErrorIs(t, send(t, client, http.MethodPost, key), providerposture.ErrDisarmed)
	require.Len(t, stripe.Submitted("/v1/customers"), 1, "the next write is refused before it is sent")
}

// A live restricted key proves its account and nothing else: its mode is its
// prefix, and it may lack the balance read.
func TestALiveRestrictedKeyIsProvenByItsAccountRead(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	verdict, err := stripeapi.PostureCheck(stripe, "rk_live_acct_restricted", "acct_restricted")(t.Context())
	require.NoError(t, err)
	require.Equal(t, providerposture.Live, verdict)
	require.Equal(t, 1, stripe.Reads("/v1/account"))
	require.Zero(t, stripe.Reads("/v1/balance"))
}

// A key Stripe refuses, or one lacking a read's permission, is refused for
// good with Stripe's reason, never the key it quotes.
func TestARefusedPostureReadDisarmsNamingThePermission(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		wire := wireFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"error":{"type":"invalid_request_error","message":"The provided key 'rk_test_*****1234' does not have the required permissions for this endpoint. Having the 'rak_balance_read' permission would allow this request to continue."}}`
			if r.URL.Path == "/v1/account" {
				account := `{"object":"account","id":"acct_refused"}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(account)), Header: http.Header{}, Request: r}, nil
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
		})
		verdict, err := stripeapi.PostureCheck(wire, "rk_test_refused", "acct_refused")(context.Background())
		require.Equal(t, providerposture.Refused, verdict, status)
		require.ErrorContains(t, err, "rak_balance_read")
		require.ErrorContains(t, err, "/v1/balance")
		require.NotContains(t, err.Error(), "rk_test_")
	}
}

// held is the write posture of each merchant it names; any other is full.
type held map[uuid.UUID]controls.Mode

func (h held) Posture(_ context.Context, id uuid.UUID) controls.Posture {
	if mode, ok := h[id]; ok {
		return controls.Posture{Mode: mode, Reason: "held for the test"}
	}
	return controls.Posture{Mode: controls.Full}
}

// A merchant's readonly write posture stops its Stripe writes at the
// transport, before a byte is sent; another merchant's writes go out, and
// the readonly merchant's reads still do.
func TestAReadonlyMerchantsStripeWritesStopAtTheTransport(t *testing.T) {
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	readonly, full := uuid.New(), uuid.New()
	factory := stripeapi.NewWiredFactory(stripe, held{readonly: controls.ReadOnly})
	client := factory.Client(0)
	name := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := "sk_test_acct_" + name
	sendAs := func(merchantID uuid.UUID, method string) error {
		req, err := http.NewRequestWithContext(merchant.WithID(t.Context(), billing.MerchantID(merchantID)), method, stripeapi.APIBase+"/v1/customers", strings.NewReader("email=a%40example.com"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}
	for _, id := range []uuid.UUID{readonly, full} {
		factory.RegisterPosture("held/"+id.String(), id, uuid.New(), "acct_"+name, key)
	}

	err := sendAs(readonly, http.MethodPost)
	require.ErrorIs(t, err, stripeapi.ErrProviderReadOnly)
	require.ErrorContains(t, err, "held for the test")
	require.Empty(t, stripe.Submitted("/v1/customers"))
	require.NoError(t, sendAs(readonly, http.MethodGet))
	require.NoError(t, sendAs(full, http.MethodPost))
	require.Len(t, stripe.Submitted("/v1/customers"), 1)
}
