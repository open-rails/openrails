package solana

import (
	"context"
	"errors"
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

	subscribeSession uuid.UUID
	subscribeSig     string
	subscribeCalls   int
	subscribeErr     error
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

func (f *fakeLifecycleMarker) ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error {
	f.subscribeCalls++
	f.subscribeSession = sessionID
	f.subscribeSig = signature
	return f.subscribeErr
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

func TestVerifyPayment_SubscribeSkipsTokenChecks(t *testing.T) {
	p := &SolanaPayPoller{} // no transaction service / RPC needed for a subscribe record
	pending := &PendingSolanaPayment{Subscribe: true}
	if !p.verifyPayment(context.Background(), "ref", "sig", pending) {
		t.Error("subscribe pending must verify on reference match alone (no token state)")
	}
}

// A confirmed recurring-subscribe reference routes to ConfirmSolanaSubscribeSession
// (enroll), NOT to a purchase registration or the lifecycle mirror.
func TestProcessConfirmedPayment_SubscribeRoutesToEnroll(t *testing.T) {
	marker := &fakeLifecycleMarker{}
	sessionID := uuid.New()
	p := &SolanaPayPoller{checkoutSessionService: marker}

	pending := &PendingSolanaPayment{
		SessionID: sessionID.String(),
		Subscribe: true,
	}
	if err := p.processConfirmedPayment(context.Background(), "ref", "sub-sig", pending); err != nil {
		t.Fatalf("processConfirmedPayment: %v", err)
	}
	if marker.subscribeCalls != 1 {
		t.Fatalf("expected exactly one ConfirmSolanaSubscribeSession call, got %d", marker.subscribeCalls)
	}
	if marker.subscribeSession != sessionID {
		t.Errorf("subscribe session = %s, want %s", marker.subscribeSession, sessionID)
	}
	if marker.subscribeSig != "sub-sig" {
		t.Errorf("subscribe signature = %q, want sub-sig", marker.subscribeSig)
	}
	if marker.confirmCalls != 0 {
		t.Error("a subscribe must not route to the lifecycle mirror")
	}
}

// When only the init tx has landed, ConfirmSolanaSubscribeSession returns
// ErrSolanaSubscribePending; processConfirmedPayment must propagate it so the
// poller keeps the reference alive (does NOT consume it) and re-polls.
func TestProcessConfirmedPayment_SubscribePendingPropagates(t *testing.T) {
	marker := &fakeLifecycleMarker{subscribeErr: ErrSolanaSubscribePending}
	sessionID := uuid.New()
	p := &SolanaPayPoller{checkoutSessionService: marker}

	pending := &PendingSolanaPayment{
		SessionID: sessionID.String(),
		Subscribe: true,
	}
	err := p.processConfirmedPayment(context.Background(), "ref", "init-sig", pending)
	if !errors.Is(err, ErrSolanaSubscribePending) {
		t.Fatalf("want ErrSolanaSubscribePending, got %v", err)
	}
}
