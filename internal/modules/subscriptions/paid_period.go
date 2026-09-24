package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/internal/db/models"
)

// PaidPeriodKey names, in a subscription charge's payment metadata, the start
// of the period the charge paid for. Two captured charges for one period of
// one subscription are a duplicate; charges for distinct periods never are.
const PaidPeriodKey = "period_start"

func withPaidPeriod(metadata map[string]any, start time.Time) map[string]any {
	out := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	out[PaidPeriodKey] = start.UTC().Format(time.RFC3339)
	return out
}

// renewalPeriodStart is the start of the period a renewal charge pays for:
// the accepted terms' period, the provider's stated start, else the end of
// the period it follows.
func renewalPeriodStart(params *RenewMembershipParams, sub *models.Subscription, now time.Time) time.Time {
	switch {
	case params.Prepared != nil:
		return params.Prepared.PeriodStart
	case params.CurrentPeriodStartsAt != nil && !params.CurrentPeriodStartsAt.IsZero():
		return *params.CurrentPeriodStartsAt
	case sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero():
		return *sub.CurrentPeriodEndsAt
	default:
		return now
	}
}
