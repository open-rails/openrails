package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// fakeConfirmRPC stubs WatchTransaction: it returns the configured outcome/err
// and records that it was called.
type fakeConfirmRPC struct {
	outcome *solanaint.TransactionOutcome
	err     error
	called  bool
	gotSig  solanago.Signature
	gotComm rpc.CommitmentType
}

func (f *fakeConfirmRPC) WatchTransaction(_ context.Context, sig solanago.Signature, comm rpc.CommitmentType, _ time.Duration) (*solanaint.TransactionOutcome, error) {
	f.called = true
	f.gotSig = sig
	f.gotComm = comm
	return f.outcome, f.err
}

// fakeCanceller stubs CancelMembership: it records the params it was called with.
type fakeCanceller struct {
	called bool
	params *subscriptions.CancelMembershipParams
	err    error
}

func (f *fakeCanceller) CancelMembership(_ context.Context, params *subscriptions.CancelMembershipParams) error {
	f.called = true
	f.params = params
	return f.err
}

// validSig is a syntactically valid base58 signature (64 zero bytes) for tests.
var validSig = solanago.Signature{}.String()

func TestConfirmCancel_Success_MirrorsAndCancels(t *testing.T) {
	rpcStub := &fakeConfirmRPC{outcome: &solanaint.TransactionOutcome{Err: nil}} // Err nil -> Succeeded
	canceller := &fakeCanceller{}
	svc := NewConfirmCancelService(rpcStub, canceller)

	subID := uuid.New()
	if err := svc.Confirm(context.Background(), subID, validSig); err != nil {
		t.Fatalf("Confirm returned error on success path: %v", err)
	}

	if !rpcStub.called {
		t.Fatal("expected WatchTransaction to be called")
	}
	if rpcStub.gotComm != rpc.CommitmentConfirmed {
		t.Fatalf("expected commitment Confirmed, got %v", rpcStub.gotComm)
	}
	if !canceller.called {
		t.Fatal("expected CancelMembership to be called (mirror step)")
	}
	if canceller.params == nil || canceller.params.SubscriptionID == nil || *canceller.params.SubscriptionID != subID {
		t.Fatalf("expected cancel for subscription %s, got %+v", subID, canceller.params)
	}
	if !canceller.params.RevokeAccess {
		t.Fatal("Solana cancel must be IMMEDIATE (RevokeAccess=true), not period-end deferred")
	}
	if canceller.params.CancelType != models.CancelTypeUser {
		t.Fatalf("expected CancelTypeUser, got %v", canceller.params.CancelType)
	}
}

func TestConfirmCancel_OnChainFailure_DoesNotCancel(t *testing.T) {
	// Landed but reverted on-chain: Err non-nil -> Succeeded() == false.
	rpcStub := &fakeConfirmRPC{outcome: &solanaint.TransactionOutcome{Err: map[string]any{"InstructionError": []any{0, "Custom"}}}}
	canceller := &fakeCanceller{}
	svc := NewConfirmCancelService(rpcStub, canceller)

	err := svc.Confirm(context.Background(), uuid.New(), validSig)
	if err == nil {
		t.Fatal("expected error for a reverted on-chain cancel")
	}
	if canceller.called {
		t.Fatal("must NOT mirror/cancel when the on-chain cancel did not succeed")
	}
}

func TestConfirmCancel_NotConfirmed_DoesNotCancel(t *testing.T) {
	// Never confirmed (timeout / RPC down): WatchTransaction returns an error.
	rpcStub := &fakeConfirmRPC{err: errors.New("watch transaction: context deadline exceeded")}
	canceller := &fakeCanceller{}
	svc := NewConfirmCancelService(rpcStub, canceller)

	err := svc.Confirm(context.Background(), uuid.New(), validSig)
	if err == nil {
		t.Fatal("expected error when the signature never confirms")
	}
	if canceller.called {
		t.Fatal("must NOT mirror/cancel when the signature was never confirmed")
	}
}

func TestConfirmCancel_InvalidSignature(t *testing.T) {
	rpcStub := &fakeConfirmRPC{}
	canceller := &fakeCanceller{}
	svc := NewConfirmCancelService(rpcStub, canceller)

	if err := svc.Confirm(context.Background(), uuid.New(), "not-a-valid-base58-sig!!!"); err == nil {
		t.Fatal("expected error for an invalid signature")
	}
	if rpcStub.called {
		t.Fatal("must not watch an invalid signature")
	}
	if canceller.called {
		t.Fatal("must not cancel for an invalid signature")
	}
}

func TestConfirmCancel_EmptyInputs(t *testing.T) {
	svc := NewConfirmCancelService(&fakeConfirmRPC{}, &fakeCanceller{})
	if err := svc.Confirm(context.Background(), uuid.Nil, validSig); err == nil {
		t.Fatal("expected error for nil subscription id")
	}
	if err := svc.Confirm(context.Background(), uuid.New(), ""); err == nil {
		t.Fatal("expected error for empty signature")
	}
}
