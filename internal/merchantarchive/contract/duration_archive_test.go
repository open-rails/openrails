package contract

import (
	"strconv"
	"testing"

	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestArchiveDurationCannotOverflowRuntime(t *testing.T) {
	for _, field := range []struct{ table, column string }{
		{"prices", "access_duration_hours"}, {"prices", "billing_interval_hours"},
		{"prices", "trial_duration_hours"}, {"subscriptions", "access_duration_hours_snapshot"},
	} {
		for _, hours := range []int{0, -1, catalog.MaxDurationHours, catalog.MaxDurationHours + 1} {
			fields := map[string]string{field.column: strconv.Itoa(hours)}
			if field.table == "subscriptions" {
				fields["collection_policy"], fields["rail"], fields["rail_subscription_id"] = "provider", "stripe", "sub_legacy"
			}
			profile, values := row(t, field.table, fields)
			err := ValidateValues(profile, values)
			if hours == catalog.MaxDurationHours {
				require.NoError(t, err, field.column)
			} else {
				require.Error(t, err, field.column)
			}
		}
	}
}
