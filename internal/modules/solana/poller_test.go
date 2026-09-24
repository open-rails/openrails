package solana

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

type sessionMarker struct {
	lifecycle, subscribe, succeeded int
	session                         uuid.UUID
	signature                       string
	err                             error
}

func (m *sessionMarker) MarkSucceeded(context.Context, uuid.UUID, uuid.UUID, string) error {
	m.succeeded++
	return nil
}

func (m *sessionMarker) ConfirmSolanaLifecycleSession(_ context.Context, id uuid.UUID, sig string) error {
	m.lifecycle++
	m.session, m.signature = id, sig
	return m.err
}

func (m *sessionMarker) ConfirmSolanaSubscribeSession(_ context.Context, id uuid.UUID, sig string) error {
	m.subscribe++
	m.session, m.signature = id, sig
	return m.err
}

// Lifecycle (cancel/tier-change) and recurring-subscribe references carry no
// transfer: the random reference proves the tx, and the confirmed signature is
// routed to its session mirror, never to purchase registration.
func TestConfirmedNonPurchaseReferencesRouteToSessionMirror(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		pending       PendingSolanaPayment
		wantLifecycle int
		wantSubscribe int
	}{
		{"lifecycle", PendingSolanaPayment{Lifecycle: true}, 1, 0},
		{"subscribe", PendingSolanaPayment{Subscribe: true}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := uuid.New()
			pending := tc.pending
			pending.SessionID = " " + sessionID.String() + " "

			marker := &sessionMarker{}
			p := &SolanaPayPoller{checkoutSessionService: marker}
			require.True(t, p.verifyPayment(ctx, nil, "ref", "sig", &pending), "no token checks for a non-purchase reference")
			require.NoError(t, p.processConfirmedPayment(ctx, nil, "ref", "the-sig", &pending))
			require.Equal(t, tc.wantLifecycle, marker.lifecycle)
			require.Equal(t, tc.wantSubscribe, marker.subscribe)
			require.Zero(t, marker.succeeded)
			require.Equal(t, sessionID, marker.session)
			require.Equal(t, "the-sig", marker.signature)

			// The mirror's error (e.g. ErrSolanaSubscribePending while only init
			// landed) propagates so the poller keeps the reference alive.
			marker.err = ErrSolanaSubscribePending
			require.ErrorIs(t, p.processConfirmedPayment(ctx, nil, "ref", "the-sig", &pending), ErrSolanaSubscribePending)

			bad := pending
			bad.SessionID = "not-a-uuid"
			calls := marker.lifecycle + marker.subscribe
			require.Error(t, p.processConfirmedPayment(ctx, nil, "ref", "sig", &bad))
			require.Equal(t, calls, marker.lifecycle+marker.subscribe)

			require.Error(t, (&SolanaPayPoller{}).processConfirmedPayment(ctx, nil, "ref", "sig", &pending),
				"an unwired session service must error, not drop the confirmation")
		})
	}
}

// A replayed payment row matches the pending reference only when reference,
// session, customer, price, amount and currency all agree.
func TestSolanaPaymentMatchesPending(t *testing.T) {
	priceID := uuid.New()
	newPending := func() *PendingSolanaPayment {
		return &PendingSolanaPayment{UserID: "user_123", PriceID: priceID.String(), SessionID: "session_123", Amount: 1000, Currency: "USD"}
	}
	newPayment := func() *models.Payment {
		return &models.Payment{
			CustomerID: identity.CustomerIDFromString("user_123").UUID(),
			PriceID:    priceID,
			Amount:     1000,
			Currency:   "usd",
			Metadata:   map[string]any{"solana_reference": "reference_123", "checkout_session_id": "session_123"},
		}
	}
	require.True(t, solanaPaymentMatchesPending(newPayment(), "reference_123", newPending()))

	for name, mutate := range map[string]func(*models.Payment, *PendingSolanaPayment) string{
		"other reference": func(*models.Payment, *PendingSolanaPayment) string { return "other" },
		"other session": func(p *models.Payment, _ *PendingSolanaPayment) string {
			p.Metadata["checkout_session_id"] = "other"
			return "reference_123"
		},
		"pending without session": func(_ *models.Payment, pp *PendingSolanaPayment) string {
			pp.SessionID = ""
			return "reference_123"
		},
		"other amount": func(p *models.Payment, _ *PendingSolanaPayment) string { p.Amount = 999; return "reference_123" },
		"other currency": func(p *models.Payment, _ *PendingSolanaPayment) string {
			p.Currency = "EUR"
			return "reference_123"
		},
		"other customer": func(_ *models.Payment, pp *PendingSolanaPayment) string {
			pp.UserID = "user_456"
			return "reference_123"
		},
		"other price": func(p *models.Payment, _ *PendingSolanaPayment) string {
			p.PriceID = uuid.New()
			return "reference_123"
		},
	} {
		payment, pending := newPayment(), newPending()
		ref := mutate(payment, pending)
		require.False(t, solanaPaymentMatchesPending(payment, ref, pending), name)
	}
	require.False(t, solanaPaymentMatchesPending(nil, "reference_123", newPending()))
}

// or#893: `<merchant_id>|<reference>` is the only pending-set member shape.
func TestPendingReferenceMemberRoundTrip(t *testing.T) {
	mid := merchant.ID(uuid.New())
	gotMID, ref, err := parsePendingReferenceMember(pendingReferenceMember(mid, "RefBase58"))
	require.NoError(t, err)
	require.Equal(t, mid, gotMID)
	require.Equal(t, "RefBase58", ref)

	for _, member := range []string{
		"RefBase58",
		"not-a-uuid|RefBase58",
		uuid.Nil.String() + "|RefBase58",
		mid.String() + "|  ",
	} {
		_, _, err := parsePendingReferenceMember(member)
		require.Error(t, err, member)
	}
}

func TestGeneratePaymentRequiresCheckoutSession(t *testing.T) {
	nilID := uuid.Nil
	for _, id := range []*uuid.UUID{nil, &nilID} {
		_, err := (&SolanaPayService{}).GeneratePayment(context.Background(), "user_123", uuid.New(), "USDC", id)
		require.ErrorContains(t, err, "checkout session id is required")
	}
}
