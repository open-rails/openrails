package ccbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

func TestSandboxCCBillMutationsRequireLoopbackFixture(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<results>1</results>"))
	}))
	defer srv.Close()
	c := &DataLinkClient{BaseURL: srv.URL, ClientAccNum: "900000", ClientSubAcc: "0000", Username: "u", Password: "p", DevMode: true, HTTPClient: srv.Client()}
	_, err := c.CancelSubscription(context.Background(), "sub-1")
	require.ErrorIs(t, err, providerposture.ErrDisarmed, "CCBill has no authoritative test signal")
	require.EqualValues(t, 0, hits.Load())

	c.LoopbackFixture = true
	_, err = c.CancelSubscription(context.Background(), "sub-1")
	require.NoError(t, err)
	c.BaseURL = "https://datalink.ccbill.com"
	_, err = c.CancelSubscription(context.Background(), "sub-1")
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.EqualValues(t, 1, hits.Load())
}
