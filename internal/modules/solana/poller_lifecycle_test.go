package solana

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// fakeLifecycleMarker records the calls the poller makes to the checkout session
// service for a lifecycle (cancel / tier-change) confirmation.
type fakeLifecycleMarker struct {
	confirmedSession uuid.UUID
	confirmedSig     string
	confirmCalls     int
	markCalls        int
	confirmErr       error
}

func (f *fakeLifecycleMarker) MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error {
	f.markCalls++
	return nil
}

func (f *fakeLifecycleMarker) ConfirmSolanaLifecycleSession(ctx context.Context, sessionID uuid.UUID, signature string) error {
	f.confirmCalls++
	f.confirmedSession = sessionID
	f.confirmedSig = signature
	return f.confirmErr
}

func TestVerifyPayment_LifecycleSkipsTokenChecks(t *testing.T) {
	p := &SolanaPayPoller{} // no transaction service / RPC needed for a lifecycle record
	pending := &PendingSolanaPayment{Lifecycle: true}
	if !p.verifyPayment(context.Background(), "ref", "sig", pending) {
		t.Error("lifecycle pending must verify on reference match alone (no token state)")
	}
}

func TestProcessConfirmedPayment_LifecycleRoutesToConfirm(t *testing.T) {
	marker := &fakeLifecycleMarker{}
	sessionID := uuid.New()
	p := &SolanaPayPoller{checkoutSessionService: marker}

	pending := &PendingSolanaPayment{
		SessionID: sessionID.String(),
		Lifecycle: true,
	}
	if err := p.processConfirmedPayment(context.Background(), "ref", "the-signature", pending); err != nil {
		t.Fatalf("processConfirmedPayment: %v", err)
	}
	if marker.confirmCalls != 1 {
		t.Fatalf("expected exactly one ConfirmSolanaLifecycleSession call, got %d", marker.confirmCalls)
	}
	if marker.confirmedSession != sessionID {
		t.Errorf("confirmed session = %s, want %s", marker.confirmedSession, sessionID)
	}
	if marker.confirmedSig != "the-signature" {
		t.Errorf("confirmed signature = %q, want the-signature", marker.confirmedSig)
	}
	// A lifecycle confirmation must NOT register a purchase (no purchaseRegistrar
	// configured here would panic/error otherwise).
}
