package subscriptions

import (
	"errors"

	"github.com/google/uuid"
)

// ProviderAccountCutoverDisposition is an operator-facing classification. A
// cross-account plan is deliberately report-only until a rail proves the full
// create/verify/cancel/repoint transaction for its provider API.
type ProviderAccountCutoverDisposition string

const (
	ProviderAccountCutoverSameAccount     ProviderAccountCutoverDisposition = "same_account_durable_update"
	ProviderAccountCutoverRequiresReentry ProviderAccountCutoverDisposition = "cross_account_requires_card_reentry"
	ProviderAccountCutoverBlocked         ProviderAccountCutoverDisposition = "blocked"
)

// ProviderAccountCutoverRequest contains only provider identity and workflow
// facts. It intentionally has no card data, vault secret, or provider token.
type ProviderAccountCutoverRequest struct {
	SourcePSPID              uuid.UUID
	TargetPSPID              uuid.UUID
	SourceArchived           bool
	TargetArchived           bool
	ReplacementCardCollected bool
}

// ProviderAccountCutoverPlan is safe to serialize into an operator report.
// Executable is false for cross-account plans until provider-specific
// semantics are qualified; callers must not treat this report as completion.
type ProviderAccountCutoverPlan struct {
	Disposition ProviderAccountCutoverDisposition `json:"disposition"`
	Executable  bool                              `json:"executable"`
	Reason      string                            `json:"reason"`
	Steps       []string                          `json:"steps"`
}

var ErrProviderAccountCutoverNotQualified = errors.New("cross-provider-account cutover is not qualified for automatic execution")

// PlanProviderAccountCutover classifies one subscriber's requested move. The
// same-account case delegates to the existing durable nmi_payment_source_update
// path. A cross-account move is report-only: OpenRails cannot copy a provider
// vault token and does not claim that a generic provider API can atomically
// transfer a recurring obligation.
func PlanProviderAccountCutover(req ProviderAccountCutoverRequest) ProviderAccountCutoverPlan {
	if req.SourcePSPID == uuid.Nil || req.TargetPSPID == uuid.Nil {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverBlocked,
			Reason:      "source and target provider-account identities are required",
			Steps:       []string{"Resolve both immutable PSP identities before attempting a cutover."},
		}
	}
	if req.SourcePSPID == req.TargetPSPID {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverSameAccount,
			Executable:  true,
			Reason:      "replacement method belongs to the subscription's provider account",
			Steps: []string{
				"Use the existing durable nmi_payment_source_update intent.",
				"Read and verify the recurring record before finalizing the local payment-method link.",
			},
		}
	}
	if req.SourceArchived == false {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverBlocked,
			Reason:      "cross-account cutover is only a drain operation from an archived source account",
			Steps:       []string{"Archive the source PSP through the provider-account lifecycle before planning per-user drain."},
		}
	}
	if req.TargetArchived {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverBlocked,
			Reason:      "target provider account is archived and cannot receive new work",
			Steps:       []string{"Provision or select a non-archived target PSP for card re-entry."},
		}
	}
	if !req.ReplacementCardCollected {
		return ProviderAccountCutoverPlan{
			Disposition: ProviderAccountCutoverRequiresReentry,
			Reason:      "provider vault credentials are not portable; replacement card must be collected on the active target account",
			Steps: []string{
				"Prompt the subscriber to re-enter the card through checkout on the non-archived target PSP.",
				"Do not send the archived account's vault or provider token to the target account.",
			},
		}
	}
	return ProviderAccountCutoverPlan{
		Disposition: ProviderAccountCutoverRequiresReentry,
		Reason:      ErrProviderAccountCutoverNotQualified.Error(),
		Steps: []string{
			"Collect and tokenize the replacement card on the non-archived target PSP.",
			"Create a replacement provider subscription anchored at the current period end, only if the target API supports an idempotent delayed start.",
			"Verify the exact target subscription receipt before canceling the archived source subscription.",
			"After target confirmation, repoint local subscription and payment-method PSP identities in one transaction.",
			"If any provider step lacks a read-back or idempotency contract, stop and leave the source obligation unchanged.",
		},
	}
}
