package intents

import (
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestInitialMembershipDeclineRejectsAmbiguousRawProof(t *testing.T) {
	for _, raw := range []string{
		"response=2&response=1&response_code=202&response_code=100",
		"response=2&response=2&response_code=202",
		"response=2&response_code=202&response_code=202",
		"response=2&response_code=100",
		"response=2&response_code=202&transactionid=one&transactionid=two",
		"response=2&response_code=202&responsetext=%QRAW_PROVIDER_SENTINEL",
	} {
		t.Run(raw, func(t *testing.T) {
			// Invalid provider facts must be rejected before touching custody storage.
			var store *Store
			err := store.RetainInitialMembershipDecline(t.Context(), gen.OpenrailsRailIntent{}, &nmi.CustomerVaultError{ResponseCode: 202, RawResponse: raw})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "RAW_PROVIDER_SENTINEL")
		})
	}
}
