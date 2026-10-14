package nmi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A recurring report row as a test-mode gateway answered it (sanitized).
const recurringReportFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response><subscription id="11494735091"><subscription_id>11494735091</subscription_id><plan><plan_id>basic_monthly</plan_id><plan_name>Basic Monthly</plan_name><plan_amount>9.99</plan_amount><plan_payments>until_canceled</plan_payments><day_frequency>30</day_frequency></plan><next_charge_date>2026-10-15</next_charge_date><completed_payments>0</completed_payments><attempted_payments>0</attempted_payments><remaining_payments>until_canceled</remaining_payments><ponumber>c2882479-a07d-4f42-980d-895e90ea4060</ponumber><orderid>c2882479-a07d-4f42-980d-895e90ea4060</orderid><order_description>Subscription</order_description><first_name>Ada</first_name><last_name>Example</last_name><email>ada@example.com</email><cc_number>4xxxxxxxxxxx1111</cc_number><cc_exp>1030</cc_exp><cc_bin>411111</cc_bin></subscription></nm_response>`

func TestParseRecurringReport(t *testing.T) {
	rows, err := ParseRecurringReport(recurringReportFixture)
	require.NoError(t, err)
	require.Equal(t, []RecurringReportRow{{SubscriptionID: "11494735091", OrderID: "c2882479-a07d-4f42-980d-895e90ea4060", PONumber: "c2882479-a07d-4f42-980d-895e90ea4060",
		Email: "ada@example.com", FirstName: "Ada", LastName: "Example", PlanID: "basic_monthly"}}, rows)
	_, err = ParseRecurringReport(`<nm_response><error_response>Invalid Security Key</error_response></nm_response>`)
	require.ErrorContains(t, err, "Invalid Security Key")
}

// A transaction report names the charged vault customerid.
func TestQueryTransactionVault(t *testing.T) {
	report, err := ParseTransactionReport(`<nm_response><transaction><transaction_id>1</transaction_id><customerid>1282907112</customerid></transaction></nm_response>`)
	require.NoError(t, err)
	require.Equal(t, "1282907112", report.Transactions[0].Vault())
}
