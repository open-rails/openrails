package operator

import "github.com/open-rails/openrails/internal/modules/subscriptions"

// Provider-account cutover reports are intentionally exposed through the
// operator package as report-only types. Cross-PSP execution remains disabled
// until a provider-specific create/verify/cancel/repoint contract is proven.
type (
	ProviderAccountCutoverDisposition = subscriptions.ProviderAccountCutoverDisposition
	ProviderAccountCutoverRequest     = subscriptions.ProviderAccountCutoverRequest
	ProviderAccountCutoverPlan        = subscriptions.ProviderAccountCutoverPlan
)

const (
	ProviderAccountCutoverSameAccount     = subscriptions.ProviderAccountCutoverSameAccount
	ProviderAccountCutoverRequiresReentry = subscriptions.ProviderAccountCutoverRequiresReentry
	ProviderAccountCutoverBlocked         = subscriptions.ProviderAccountCutoverBlocked
)

func PlanProviderAccountCutover(req ProviderAccountCutoverRequest) ProviderAccountCutoverPlan {
	return subscriptions.PlanProviderAccountCutover(req)
}
