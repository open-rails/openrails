package subscriptions

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// ProviderAccountCutoverDisposition is the path a subscriber's move would take.
// Only the same-account durable payment-source update can execute; a
// cross-account plan is report-only until a rail proves the full
// create/verify/cancel/repoint transaction for its provider API (#657).
type ProviderAccountCutoverDisposition string

const (
	ProviderAccountCutoverSameAccount     ProviderAccountCutoverDisposition = "same_account_durable_update"
	ProviderAccountCutoverRequiresReentry ProviderAccountCutoverDisposition = "cross_account_requires_card_reentry"
	ProviderAccountCutoverBlocked         ProviderAccountCutoverDisposition = "blocked"
)

// ProviderAccountCutoverCode is the machine reason of a plan. Executable plans
// carry ProviderAccountCutoverReady; every other code says what blocks it.
type ProviderAccountCutoverCode string

const (
	ProviderAccountCutoverReady                     ProviderAccountCutoverCode = "ready"
	ProviderAccountCutoverIdentityMissing           ProviderAccountCutoverCode = "identity_missing"
	ProviderAccountCutoverRailUnsupported           ProviderAccountCutoverCode = "rail_unsupported"
	ProviderAccountCutoverTargetRailMismatch        ProviderAccountCutoverCode = "target_rail_mismatch"
	ProviderAccountCutoverSubscriptionNotRebilling  ProviderAccountCutoverCode = "subscription_not_rebilling"
	ProviderAccountCutoverSubscriptionNotAtProvider ProviderAccountCutoverCode = "subscription_not_at_provider"
	ProviderAccountCutoverTargetArchived            ProviderAccountCutoverCode = "target_archived"
	ProviderAccountCutoverSourceNotArchived         ProviderAccountCutoverCode = "source_not_archived"
	ProviderAccountCutoverReplacementCardRequired   ProviderAccountCutoverCode = "replacement_card_required"
	ProviderAccountCutoverReplacementCardNotFound   ProviderAccountCutoverCode = "replacement_card_not_found"
	ProviderAccountCutoverReplacementCardNotOwned   ProviderAccountCutoverCode = "replacement_card_not_owned"
	ProviderAccountCutoverReplacementCardUnusable   ProviderAccountCutoverCode = "replacement_card_unusable"
	ProviderAccountCutoverReplacementCardPSP        ProviderAccountCutoverCode = "replacement_card_psp_mismatch"
	ProviderAccountCutoverCrossAccountNotQualified  ProviderAccountCutoverCode = "cross_account_not_qualified"
)

// ProviderAccountCutoverRequest carries every fact the durable update path
// depends on, and nothing else: no card data, vault secret or provider token.
type ProviderAccountCutoverRequest struct {
	Rail                models.Rail
	Status              models.SubscriptionStatus
	HasRailSubscription bool
	SourcePSPID         uuid.UUID
	SourceArchived      bool
	TargetPSPID         uuid.UUID
	TargetRail          models.Rail
	TargetArchived      bool
	// Replacement is the card the subscriber re-entered; nil = none yet.
	Replacement *ProviderAccountCutoverReplacement
}

// ProviderAccountCutoverReplacement is the replacement method's readiness.
type ProviderAccountCutoverReplacement struct {
	Found bool
	// OwnedByPayer: the method belongs to the subscription's customer.
	OwnedByPayer bool
	PSPID        uuid.UUID
	Rail         models.Rail
	// PSPVaulted: held in the PSP's own vault with a vault reference — what
	// the NMI update addresses (customer_vault_id).
	PSPVaulted bool
	Parked     bool
}

// ProviderAccountCutoverPlan is safe to serialize into an operator report.
// Executable is true only for the supported, fully prepared path; callers must
// never treat a report as completion.
type ProviderAccountCutoverPlan struct {
	Disposition ProviderAccountCutoverDisposition `json:"disposition"`
	Executable  bool                              `json:"executable"`
	Code        ProviderAccountCutoverCode        `json:"code"`
	Reason      string                            `json:"reason"`
	Steps       []string                          `json:"steps,omitempty"`
}

var ErrProviderAccountCutoverNotQualified = errors.New("cross-provider-account cutover is not qualified for automatic execution")

func blocked(code ProviderAccountCutoverCode, reason string) ProviderAccountCutoverPlan {
	return ProviderAccountCutoverPlan{Disposition: ProviderAccountCutoverBlocked, Code: code, Reason: reason}
}

// PlanProviderAccountCutover classifies one subscriber's requested move. It
// reports Executable only when the existing durable nmi_payment_source_update
// would accept it: an NMI subscription that still rebills at the provider, a
// non-archived target equal to the subscription's account, and a replacement
// card the payer owns, vaulted by that account and usable. A cross-account move
// is report-only: OpenRails cannot copy a provider vault token and does not
// claim a generic provider API can atomically transfer a recurring obligation.
func PlanProviderAccountCutover(req ProviderAccountCutoverRequest) ProviderAccountCutoverPlan {
	switch {
	case req.SourcePSPID == uuid.Nil || req.TargetPSPID == uuid.Nil:
		return blocked(ProviderAccountCutoverIdentityMissing, "source and target provider-account identities are required")
	case !rails.IsNMI(req.Rail):
		return blocked(ProviderAccountCutoverRailUnsupported,
			fmt.Sprintf("rail %q has no durable payment-source update; only NMI subscriptions can change their billing card", req.Rail))
	case req.Status != models.StatusActive && req.Status != models.StatusPastDue && req.Status != models.StatusAwaitingMethod:
		return blocked(ProviderAccountCutoverSubscriptionNotRebilling,
			fmt.Sprintf("subscription is %s; only active or past_due subscriptions rebill", req.Status))
	case !req.HasRailSubscription:
		return blocked(ProviderAccountCutoverSubscriptionNotAtProvider, "subscription has no provider recurring record to repoint")
	}
	if r := req.Replacement; r != nil && !r.Found {
		return blocked(ProviderAccountCutoverReplacementCardNotFound, "replacement payment method does not exist")
	} else if r != nil && !r.OwnedByPayer {
		return blocked(ProviderAccountCutoverReplacementCardNotOwned, "replacement payment method belongs to another customer")
	}
	switch {
	case !rails.SameRail(req.TargetRail, req.Rail):
		return blocked(ProviderAccountCutoverTargetRailMismatch,
			fmt.Sprintf("target provider account is on rail %q, the subscription on %q", req.TargetRail, req.Rail))
	case req.TargetArchived:
		return blocked(ProviderAccountCutoverTargetArchived, "target provider account is archived and cannot receive new work; collect the card on an active account")
	}
	if r := req.Replacement; r != nil {
		switch {
		case r.PSPID != req.TargetPSPID:
			return blocked(ProviderAccountCutoverReplacementCardPSP, "replacement payment method was vaulted by a different provider account than the target")
		case !rails.SameRail(r.Rail, req.Rail) || !r.PSPVaulted || r.Parked:
			return blocked(ProviderAccountCutoverReplacementCardUnusable, "replacement payment method is not a usable PSP-vaulted card on this rail")
		}
	}
	if req.SourcePSPID == req.TargetPSPID {
		if req.Replacement == nil {
			return ProviderAccountCutoverPlan{
				Disposition: ProviderAccountCutoverSameAccount,
				Code:        ProviderAccountCutoverReplacementCardRequired,
				Reason:      "no replacement card has been collected on the subscription's provider account",
				Steps:       []string{"Collect the replacement card on the subscription's provider account, then re-plan with its payment method id."},
			}
		}
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverSameAccount,
			Executable:  true,
			Code:        ProviderAccountCutoverReady,
			Reason:      "replacement card is vaulted by the subscription's provider account",
			Steps: []string{
				"PUT /v1/merchant/subscriptions/{id}/payment-method with the replacement payment method (durable nmi_payment_source_update intent).",
				"The intent reads the recurring record before the update and verifies any ambiguous response before finalizing the local link.",
			},
		}
	}
	if !req.SourceArchived {
		return blocked(ProviderAccountCutoverSourceNotArchived, "cross-account cutover is only a drain operation from an archived source account")
	}
	if req.Replacement == nil {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverRequiresReentry,
			Code:        ProviderAccountCutoverReplacementCardRequired,
			Reason:      "provider vault credentials are not portable; the replacement card must be collected on the active target account",
			Steps: []string{
				"Prompt the subscriber to re-enter the card through checkout on the non-archived target account.",
				"Do not send the archived account's vault or provider token to the target account.",
			},
		}
	}
	return ProviderAccountCutoverPlan{
		Disposition: ProviderAccountCutoverRequiresReentry,
		Code:        ProviderAccountCutoverCrossAccountNotQualified,
		Reason:      ErrProviderAccountCutoverNotQualified.Error(),
		Steps: []string{
			"Not executable: a cross-account move needs a qualified provider contract for each step below.",
			"Create a replacement provider subscription anchored at the current period end, only with an idempotent delayed start.",
			"Verify the exact target subscription receipt before cancelling the archived source subscription.",
			"After target confirmation, repoint local subscription and payment-method provider identities in one transaction.",
			"If any provider step lacks a read-back or idempotency contract, stop and leave the source obligation unchanged.",
		},
	}
}
