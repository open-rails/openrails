package contract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/stretchr/testify/require"
)

func archivedProviderBill() observed.Bill {
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	return observed.Bill{PSPID: uuid.MustParse(testMerchant), CustomerID: uuid.MustParse(testMerchant),
		SubscriptionID: new(uuid.MustParse(testMerchant)), Owner: "provider", Kind: "renewal", SourceKind: "stripe_invoice", SourceKey: "in_accepted", ProviderReference: "in_accepted",
		Currency: "USD", Total: 9007199254740993, PeriodStart: start, PeriodEnd: start.Add(720 * time.Hour), DueAt: start,
		Lines: []observed.Line{{PriceID: new(uuid.MustParse(testMerchant)), ProductID: new(uuid.MustParse(testMerchant)), Description: "Frozen seats", Quantity: new(3), UnitAmount: 3002399751580331, Amount: 9007199254740993, BillingIntervalHours: new(720)}}}
}

func TestProviderBillSnapshotContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*observed.Bill)
	}{
		{"missing line", func(b *observed.Bill) { b.Lines = nil }},
		{"different frozen total", func(b *observed.Bill) { b.Total-- }},
		{"unbounded lines", func(b *observed.Bill) { b.Lines = make([]observed.Line, 101) }},
		{"mutable settlement in snapshot", func(b *observed.Bill) { b.Settled = true; b.SettlementSource = "cash" }},
		{"cross-payer handle", func(b *observed.Bill) { b.ExistingOrderID = new(uuid.New()) }},
		{"missing catalog identity", func(b *observed.Bill) { b.Lines[0].PriceID = nil }},
		{"adjustment impersonates catalog", func(b *observed.Bill) { b.Lines[0].Source = "provider_adjustment" }},
		{"bad period", func(b *observed.Bill) { b.PeriodEnd = b.PeriodStart }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bill := archivedProviderBill()
			tc.mutate(&bill)
			raw, err := json.Marshal(bill)
			require.NoError(t, err)
			require.Error(t, validateJSON("provider_bills.snapshot", string(raw)))
		})
	}
	bill := archivedProviderBill()
	raw, err := json.Marshal(bill)
	require.NoError(t, err)
	require.NoError(t, validateJSON("provider_bills.snapshot", string(raw)), "amounts above 2^53 remain exact")
	require.Error(t, validateJSON("provider_bills.snapshot", strings.Replace(string(raw), `"Total":9007199254740993`, `"Total":9007199254740992`, 1)))
	require.Error(t, validateJSON("provider_bills.snapshot", strings.Replace(string(raw), `"Lines":`, `"raw_provider_body":{},"Lines":`, 1)))
	require.Error(t, validateJSON("provider_bills.snapshot", strings.Replace(string(raw), `"Total":`, `"Total":1,"Total":`, 1)))
	require.Error(t, validateJSON("provider_bills.snapshot", strings.Replace(string(raw), `Frozen seats`, strings.Repeat("x", 262144), 1)))
}

func TestProviderArchiveIdentityAndSchedule(t *testing.T) {
	bill := archivedProviderBill()
	raw, err := json.Marshal(bill)
	require.NoError(t, err)
	fields := map[string]string{"merchant_id": testMerchant, "psp_id": bill.PSPID.String(), "customer_id": bill.CustomerID.String(), "order_id": testMerchant,
		"source_kind": bill.SourceKind, "source_key": bill.SourceKey, "provider_reference": bill.ProviderReference, "snapshot": string(raw)}
	p, values := row(t, "provider_bills", fields)
	require.NoError(t, ValidateValues(p, values))
	fields["customer_id"] = uuid.NewString()
	p, values = row(t, "provider_bills", fields)
	require.ErrorContains(t, ValidateValues(p, values), "another identity")
	fields["customer_id"] = bill.CustomerID.String()
	fields["source_key"] = "in_other"
	p, values = row(t, "provider_bills", fields)
	require.ErrorContains(t, ValidateValues(p, values), "persisted identity")

	bill.SourceKind, bill.SourceKey, bill.ProviderReference = "provider_cycle", "", "provider-subscription-1"
	bill.PeriodStart, bill.PeriodEnd, bill.DueAt = time.Time{}, time.Time{}, time.Time{}
	raw, err = json.Marshal(bill)
	require.NoError(t, err)
	fields = map[string]string{"merchant_id": testMerchant, "psp_id": bill.PSPID.String(), "customer_id": bill.CustomerID.String(), "subscription_id": bill.SubscriptionID.String(), "initial_order_id": testMerchant,
		"cadence_hours": "720", "cadence_months": "0", "source_reference": "accepted_enrollment", "provider_reference": "provider-subscription-1", "accepted_version": "1", "accepted_bill": string(raw)}
	p, values = row(t, "provider_schedules", fields)
	require.NoError(t, ValidateValues(p, values))
	fields["provider_reference"] = " "
	p, values = row(t, "provider_schedules", fields)
	require.ErrorContains(t, ValidateValues(p, values), "provider reference")
	fields["provider_reference"] = "provider-subscription-1"
	fields["accepted_version"] = "0"
	p, values = row(t, "provider_schedules", fields)
	require.ErrorContains(t, ValidateValues(p, values), "accepted version")
	fields["accepted_version"] = "1"
	fields["cadence_hours"] = "24"
	p, values = row(t, "provider_schedules", fields)
	require.ErrorContains(t, ValidateValues(p, values), "cadence")
	bill.Lines[0].BillingIntervalHours = nil
	raw, err = json.Marshal(bill)
	require.NoError(t, err)
	fields["accepted_bill"], fields["cadence_hours"], fields["cadence_months"] = string(raw), "0", "1"
	delete(fields, "initial_order_id")
	p, values = row(t, "provider_schedules", fields)
	require.NoError(t, ValidateValues(p, values), "an explicit monthly forward agreement needs no invented initial order")
	fields["cadence_hours"] = "720"
	p, values = row(t, "provider_schedules", fields)
	require.ErrorContains(t, ValidateValues(p, values), "cadence", "two schedule clocks cannot be accepted")
	fields["cadence_hours"], fields["cadence_months"] = "2147483640", "0"
	bill.Lines[0].BillingIntervalHours = new(2147483640)
	raw, err = json.Marshal(bill)
	require.NoError(t, err)
	fields["accepted_bill"] = string(raw)
	p, values = row(t, "provider_schedules", fields)
	require.ErrorContains(t, ValidateValues(p, values), "cadence", "accepted cadence cannot overflow a runtime duration")
}

func TestProviderArchiveWireRetainsReplayIdentity(t *testing.T) {
	bill := archivedProviderBill()
	raw, err := json.Marshal(bill)
	require.NoError(t, err)
	p, values := row(t, "provider_bills", map[string]string{"merchant_id": testMerchant, "psp_id": bill.PSPID.String(), "customer_id": bill.CustomerID.String(), "order_id": testMerchant,
		"source_kind": bill.SourceKind, "source_key": bill.SourceKey, "provider_reference": bill.ProviderReference, "snapshot": string(raw)})
	var artifact bytes.Buffer
	writer, err := archivewire.NewWriter(&artifact, testMerchant)
	require.NoError(t, err)
	for _, table := range Profiles {
		require.NoError(t, writer.Table(table.Name))
		if table.Name == p.Name {
			require.NoError(t, writer.Row(values))
		}
	}
	require.NoError(t, writer.Close())
	seen := 0
	_, err = Read(bytes.NewReader(artifact.Bytes()), nil, func(table Profile, got []*string) error {
		require.Equal(t, p.Name, table.Name)
		require.Equal(t, values, got)
		seen++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, seen)
}

func TestProviderSettlementEventArchiveContract(t *testing.T) {
	for _, source := range []string{"cash", "credit", "out_of_band", "mixed"} {
		raw := `{"customer_id":"` + testMerchant + `","status":"complete","payment_status":"succeeded","number":2026,"settlement_source":"` + source + `"}`
		require.NoError(t, validateJSON("host_outbox.data", raw))
	}
	require.Error(t, validateJSON("host_outbox.data", `{"settlement_source":{"raw_provider_body":{}}}`))
	require.Error(t, validateJSON("host_outbox.data", `{"settlement_source":"cash","raw_provider_body":{}}`))
}
