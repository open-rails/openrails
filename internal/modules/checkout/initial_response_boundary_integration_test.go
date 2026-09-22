//go:build integration

package checkout

import (
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestInitialEnrollmentResponseBoundary(t *testing.T) {
	for _, mode := range []string{"duplicate_response", "communication_response"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.gateway.createMode.Store(mode)
			in := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
			persisted, err := fx.runner.Store.Get(fx.ctx, in.ID)
			require.NoError(t, err)
			require.NotNil(t, persisted.LastFailureReason)
			require.NotContains(t, *persisted.LastFailureReason, "RAW_PROVIDER_SENTINEL")
			require.NotContains(t, string(in.ResultEvidence), "RAW_PROVIDER_SENTINEL")
			err = intents.NewStore(fx.db).RetainInitialMembershipDecline(fx.ctx, in, &nmi.CustomerVaultError{ResponseCode: 202, RawResponse: "response=2&response=1&response_code=202&response_code=100"})
			require.Error(t, err)
			outcome := NewInitialMembershipIntentHandler(fx.svc).Verify(fx.ctx, in)
			require.NotEqual(t, intents.OutcomeTerminal, outcome.Class)
			require.EqualValues(t, 1, fx.gateway.createCalls.Load())
		})
	}
}
