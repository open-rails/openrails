package stripeapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

type fakeStripe struct {
	*httptest.Server
	livemode          atomic.Value
	account           string
	balance, accounts atomic.Int64
	writes            atomic.Int64
}

func newFakeStripe(t *testing.T, livemode, account string) *fakeStripe {
	f := &fakeStripe{account: account}
	f.livemode.Store(livemode)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/balance":
			f.balance.Add(1)
			_, _ = w.Write([]byte(`{"object":"balance","livemode":` + f.livemode.Load().(string) + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/account":
			f.accounts.Add(1)
			_, _ = w.Write([]byte(`{"object":"account","id":"` + f.account + `"}`))
		default:
			f.writes.Add(1)
			_, _ = w.Write([]byte(`{"id":"ch_1"}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// sandboxClient routes api.stripe.com to the fake without the fixture
// exemption, so the real posture gate runs.
func sandboxClient(f *fakeStripe) *http.Client {
	return &http.Client{Transport: &guardTransport{sandbox: true, base: HostRewriteTransport(f.URL)}}
}

func post(t *testing.T, c *http.Client, key string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, APIBase+"/v1/charges", strings.NewReader("amount=1"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestSandboxStripeKeyVerifiedOnceAcrossMutations(t *testing.T) {
	f := newFakeStripe(t, "false", "acct_1")
	key := "sk_test_" + uuid.NewString()
	c := sandboxClient(f)
	for i := 0; i < 3; i++ {
		require.NoError(t, post(t, c, key))
	}
	require.EqualValues(t, 1, f.balance.Load())
	require.EqualValues(t, 3, f.writes.Load())
}

func TestSandboxStripeLiveOrLiveModeDisarms(t *testing.T) {
	f := newFakeStripe(t, "true", "acct_1")
	c := sandboxClient(f)
	require.ErrorIs(t, post(t, c, "sk_test_"+uuid.NewString()), providerposture.ErrDisarmed)
	require.ErrorIs(t, post(t, c, "sk_live_"+uuid.NewString()), providerposture.ErrDisarmed)
	require.EqualValues(t, 1, f.balance.Load(), "a live key is refused without any provider read")
	require.EqualValues(t, 0, f.writes.Load())
}

func TestStripeStartupVerificationBindsDeclaredAccount(t *testing.T) {
	f := newFakeStripe(t, "false", "acct_real")
	key := "sk_test_" + uuid.NewString()
	check := PostureCheck(HostRewriteTransport(f.URL), key, APIBase, "", "acct_declared")
	status := providerposture.Process().Verify(context.Background(), PostureKey(key, APIBase, ""), check)
	require.Equal(t, providerposture.Mismatched, status.Verdict)
	require.ErrorIs(t, post(t, sandboxClient(f), key), providerposture.ErrDisarmed, "the transport gate honors the startup verdict")
	require.EqualValues(t, 0, f.writes.Load())
}

func TestLoopbackStripeFixtureStillRefusesLiveKeys(t *testing.T) {
	f := newFakeStripe(t, "true", "acct_1")
	c := &http.Client{Transport: &guardTransport{sandbox: true}}
	req, _ := http.NewRequest(http.MethodPost, f.URL+"/v1/charges", strings.NewReader("amount=1"))
	req.Header.Set("Authorization", "Bearer sk_test_fixture")
	resp, err := c.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodPost, f.URL+"/v1/charges", strings.NewReader("amount=1"))
	req.Header.Set("Authorization", "Bearer sk_live_fixture")
	_, err = c.Do(req)
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.EqualValues(t, 0, f.balance.Load())
}
