//go:build greenfield && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// One PSP whose credentials cannot be checked never takes checkout down: the
// document still lists every other PSP with its public values, and that one as
// temporarily unavailable, without them.
func TestCheckoutConfigDegradesOnePSP(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.psps
		SET evidence = jsonb_set(coalesce(evidence, '{}'::jsonb), '{settings,endpoint_deployment}', '"bogus"')
		WHERE rail = 'nmi'`))
	require.NoError(t, err)

	for _, tp := range []topology{embedded, remote} {
		cfg, err := w.client[tp].GetCheckoutConfig(t.Context())
		require.NoError(t, err, "%s: the document is served", tp)
		byRail := map[string]openrails.CheckoutPSPConfig{}
		for _, psp := range cfg.PSPs {
			byRail[psp.Rail] = psp
		}
		require.Empty(t, byRail["stripe"].Status, "%s: the other PSPs stay available", tp)
		nmi, ok := byRail["nmi"]
		require.True(t, ok, "%s: the failing PSP is still listed: %+v", tp, cfg.PSPs)
		require.Equal(t, openrails.CheckoutPSPTemporarilyUnavailable, nmi.Status, tp)
		require.Positive(t, nmi.RetryAfter, tp)
		require.Empty(t, nmi.Config, "%s: no values to drive an unavailable PSP", tp)
	}
}
