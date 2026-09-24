package subscriptions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// Only the fully prepared same-account NMI update is executable; every other
// plan carries a machine code, and blocked plans carry no instructions.
func TestPlanProviderAccountCutover(t *testing.T) {
	source, target := uuid.New(), uuid.New()
	card := func(psp uuid.UUID) *ProviderAccountCutoverReplacement {
		return &ProviderAccountCutoverReplacement{Found: true, OwnedByPayer: true, PSPID: psp, Rail: models.RailNMI, PSPVaulted: true}
	}
	for _, tc := range []struct {
		name        string
		mutate      func(*ProviderAccountCutoverRequest)
		disposition ProviderAccountCutoverDisposition
		code        ProviderAccountCutoverCode
	}{
		{"ready", func(*ProviderAccountCutoverRequest) {}, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReady},
		{"past_due ready", func(r *ProviderAccountCutoverRequest) { r.Status = models.StatusPastDue }, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReady},
		{"missing source", func(r *ProviderAccountCutoverRequest) { r.SourcePSPID = uuid.Nil }, ProviderAccountCutoverBlocked, ProviderAccountCutoverIdentityMissing},
		{"missing target", func(r *ProviderAccountCutoverRequest) { r.TargetPSPID = uuid.Nil }, ProviderAccountCutoverBlocked, ProviderAccountCutoverIdentityMissing},
		{"stripe", func(r *ProviderAccountCutoverRequest) { r.Rail = models.RailStripe }, ProviderAccountCutoverBlocked, ProviderAccountCutoverRailUnsupported},
		{"ccbill", func(r *ProviderAccountCutoverRequest) { r.Rail = models.RailCCBill }, ProviderAccountCutoverBlocked, ProviderAccountCutoverRailUnsupported},
		{"cancelled", func(r *ProviderAccountCutoverRequest) { r.Status = models.StatusCancelled }, ProviderAccountCutoverBlocked, ProviderAccountCutoverSubscriptionNotRebilling},
		{"no provider record", func(r *ProviderAccountCutoverRequest) { r.HasRailSubscription = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverSubscriptionNotAtProvider},
		{"target on another rail", func(r *ProviderAccountCutoverRequest) { r.TargetRail = models.RailStripe }, ProviderAccountCutoverBlocked, ProviderAccountCutoverTargetRailMismatch},
		{"archived target", func(r *ProviderAccountCutoverRequest) { r.TargetArchived = true }, ProviderAccountCutoverBlocked, ProviderAccountCutoverTargetArchived},
		{"no card yet", func(r *ProviderAccountCutoverRequest) { r.Replacement = nil }, ProviderAccountCutoverSameAccount, ProviderAccountCutoverReplacementCardRequired},
		{"card missing", func(r *ProviderAccountCutoverRequest) { r.Replacement = &ProviderAccountCutoverReplacement{} }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardNotFound},
		{"card not owned", func(r *ProviderAccountCutoverRequest) { r.Replacement.OwnedByPayer = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardNotOwned},
		{"card on another psp", func(r *ProviderAccountCutoverRequest) { r.Replacement.PSPID = target }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardPSP},
		{"card on another rail", func(r *ProviderAccountCutoverRequest) { r.Replacement.Rail = models.RailStripe }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardUnusable},
		{"card parked", func(r *ProviderAccountCutoverRequest) { r.Replacement.Parked = true }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardUnusable},
		{"card custodian-held", func(r *ProviderAccountCutoverRequest) { r.Replacement.PSPVaulted = false }, ProviderAccountCutoverBlocked, ProviderAccountCutoverReplacementCardUnusable},
		{"cross account, active source", func(r *ProviderAccountCutoverRequest) { r.TargetPSPID, r.Replacement = target, card(target) }, ProviderAccountCutoverBlocked, ProviderAccountCutoverSourceNotArchived},
		{"cross account, no card", func(r *ProviderAccountCutoverRequest) {
			r.TargetPSPID, r.SourceArchived, r.Replacement = target, true, nil
		}, ProviderAccountCutoverRequiresReentry, ProviderAccountCutoverReplacementCardRequired},
		{"cross account, card collected", func(r *ProviderAccountCutoverRequest) {
			r.TargetPSPID, r.SourceArchived, r.Replacement = target, true, card(target)
		}, ProviderAccountCutoverRequiresReentry, ProviderAccountCutoverCrossAccountNotQualified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ProviderAccountCutoverRequest{
				Rail: models.RailNMI, Status: models.StatusActive, HasRailSubscription: true,
				SourcePSPID: source, TargetPSPID: source, TargetRail: models.RailNMI, Replacement: card(source),
			}
			tc.mutate(&req)
			plan := PlanProviderAccountCutover(req)
			require.Equal(t, tc.disposition, plan.Disposition)
			require.Equal(t, tc.code, plan.Code)
			require.Equal(t, tc.code == ProviderAccountCutoverReady, plan.Executable)
			require.NotEmpty(t, plan.Reason)
			if plan.Disposition == ProviderAccountCutoverBlocked {
				require.Empty(t, plan.Steps)
			}
			if !plan.Executable {
				for _, step := range plan.Steps {
					require.NotContains(t, step, "payment-method with the replacement")
				}
			}
		})
	}
}

type recordingNMISource struct {
	client     *nmi.NMIClient
	err        error
	merchantID uuid.UUID
	stamped    *uuid.UUID
}

func (r *recordingNMISource) ResolveNMIClient(_ context.Context, merchantID uuid.UUID, stamped *uuid.UUID) (*nmi.NMIClient, bool, error) {
	r.merchantID, r.stamped = merchantID, stamped
	return r.client, r.client != nil, r.err
}

// #788/#655: an existing subscription resolves through its own merchant and
// stamped PSP (archived stays addressable), and every failure is closed.
func TestNMIClientForExistingSubscription(t *testing.T) {
	ctx := context.Background()
	s := &models.Subscription{Rail: models.RailNMI, MerchantID: uuid.New(), PspID: uuid.New()}
	client := &nmi.NMIClient{}
	src := &recordingNMISource{client: client}
	got, rail, ok, err := NMIClientForExistingSubscription(ctx, src, s)
	require.NoError(t, err)
	require.True(t, ok)
	require.Same(t, client, got)
	require.Equal(t, "nmi", rail)
	require.Equal(t, s.MerchantID, src.merchantID)
	require.Equal(t, s.PspID, *src.stamped)

	_, _, _, err = NMIClientForExistingSubscription(ctx, nil, s)
	require.Error(t, err)
	_, _, _, err = NMIClientForExistingSubscription(ctx, src, nil)
	require.Error(t, err)

	boom := errors.New("store unavailable")
	_, _, ok, err = NMIClientForExistingSubscription(ctx, &recordingNMISource{client: client, err: boom}, s)
	require.ErrorIs(t, err, boom)
	require.False(t, ok)

	got, _, ok, err = NMIClientForExistingSubscription(ctx, &recordingNMISource{}, s)
	require.NoError(t, err, "no armed account is a skip, not an error")
	require.False(t, ok)
	require.Nil(t, got)
}

// Vault references are account-scoped: the durable update seam refuses a card
// from another PSP and any card not held in the PSP's own vault.
func TestPaymentMethodProviderAccountBoundary(t *testing.T) {
	psp := uuid.New()
	s := &models.Subscription{PspID: psp}
	require.NoError(t, ValidatePaymentMethodProviderAccount(&models.PaymentMethod{PspID: psp}, s))
	err := ValidatePaymentMethodProviderAccount(&models.PaymentMethod{PspID: uuid.New()}, s)
	require.ErrorIs(t, err, ErrPaymentMethodProviderAccountMismatch)
	require.ErrorContains(t, err, "card re-entry")
	require.Error(t, ValidatePaymentMethodProviderAccount(nil, s))

	custodian := uuid.New()
	require.NoError(t, ValidatePaymentMethodSourceCustody(&models.PaymentMethod{Custodian: models.CustodianPSP}))
	for _, pm := range []*models.PaymentMethod{nil, {Custodian: models.CustodianPSP, CustodianID: &custodian}, {Custodian: "third_party"}} {
		require.ErrorIs(t, ValidatePaymentMethodSourceCustody(pm), ErrPaymentMethodNotPSPVaulted)
	}
}
