package checkout

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// solana_pay_url must now be surfaced for EVERY Solana-Pay-capable session, not
// just the one-off transaction_request flow: recurring subscribe (which now also
// uses flow=transaction_request) and the cancel / tier-change lifecycle modes.
func TestSessionToResponse_SolanaPayURLForAllModes(t *testing.T) {
	t.Parallel()

	cfg := testSolanaCheckoutConfig()
	cfg.APIURL = "https://api.test.com"
	svc := &CheckoutSessionService{config: cfg, rails: testSolanaCheckoutRails()}

	cases := []struct {
		name  string
		mode  models.CheckoutSessionMode
		state map[string]any
		want  bool
	}{
		{
			name:  "recurring subscribe over solana pay",
			mode:  models.CheckoutSessionModeSubscription,
			state: map[string]any{"flow": "transaction_request"},
			want:  true,
		},
		{
			name:  "one-off transfer over solana pay",
			mode:  models.CheckoutSessionModeOneOff,
			state: map[string]any{"flow": "transaction_request"},
			want:  true,
		},
		{
			name:  "solana cancel lifecycle",
			mode:  models.CheckoutSessionModeSolanaCancel,
			state: map[string]any{"flow": string(models.CheckoutSessionModeSolanaCancel)},
			want:  true,
		},
		{
			name:  "solana tier change lifecycle",
			mode:  models.CheckoutSessionModeSolanaTierChange,
			state: map[string]any{"flow": string(models.CheckoutSessionModeSolanaTierChange)},
			want:  true,
		},
		{
			name:  "wallet-connected subscribe (no transaction_request) gets no pay url",
			mode:  models.CheckoutSessionModeSubscription,
			state: map[string]any{"flow": "subscription"},
			want:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			session := &models.CheckoutSession{
				ID:        uuid.New(),
				Status:    models.CheckoutSessionStatusRequiresAction,
				Rail:      models.RailSolana,
				Mode:      tc.mode,
				RailState: tc.state,
			}
			resp := svc.sessionToResponse(session)
			require.NotNil(t, resp)
			if tc.want {
				require.NotEmpty(t, resp.Payment.SolanaPayURL, "expected a solana_pay_url")
				require.Contains(t, resp.Payment.SolanaPayURL, "/v1/checkout/")
				require.Contains(t, resp.Payment.SolanaPayURL, "/solana-pay")
			} else {
				require.Empty(t, resp.Payment.SolanaPayURL, "did not expect a solana_pay_url")
			}
		})
	}
}

func TestSolanaSessionUsesPayURL(t *testing.T) {
	t.Parallel()

	mk := func(mode models.CheckoutSessionMode, flow string) *models.CheckoutSession {
		return &models.CheckoutSession{
			Rail:      models.RailSolana,
			Mode:      mode,
			RailState: map[string]any{"flow": flow},
		}
	}

	require.True(t, solanaSessionUsesPayURL(mk(models.CheckoutSessionModeSubscription, "transaction_request")))
	require.True(t, solanaSessionUsesPayURL(mk(models.CheckoutSessionModeOneOff, "transaction_request")))
	require.True(t, solanaSessionUsesPayURL(mk(models.CheckoutSessionModeSolanaCancel, "")))
	require.True(t, solanaSessionUsesPayURL(mk(models.CheckoutSessionModeSolanaTierChange, "")))
	require.False(t, solanaSessionUsesPayURL(mk(models.CheckoutSessionModeSubscription, "subscription")))
	require.False(t, solanaSessionUsesPayURL(nil))

	// Non-solana rail never gets a solana pay url even if state looks similar.
	nonSolana := &models.CheckoutSession{
		Rail:      models.RailStripe,
		Mode:      models.CheckoutSessionModeSubscription,
		RailState: map[string]any{"flow": "transaction_request"},
	}
	require.False(t, solanaSessionUsesPayURL(nonSolana))
}

// isSolanaSubscribeNotLandedErr distinguishes the in-progress "subscribe tx not
// landed yet" enrollment error (subscription PDA unfunded → "not found on-chain")
// from a genuine failure, so the subscribe poller can keep re-polling.
func TestIsSolanaSubscribeNotLandedErr(t *testing.T) {
	t.Parallel()

	notLanded := fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run the atomic subscribe?", "PdA")
	require.True(t, isSolanaSubscribeNotLandedErr(notLanded))
	require.True(t, isSolanaSubscribeNotLandedErr(fmt.Errorf("wrap: %w", notLanded)))

	require.False(t, isSolanaSubscribeNotLandedErr(nil))
	require.False(t, isSolanaSubscribeNotLandedErr(errors.New("create membership: db down")))
}
