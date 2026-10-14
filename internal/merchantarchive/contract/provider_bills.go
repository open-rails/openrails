package contract

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Only the accepted bill is portable here. Provider bodies, payment requests
// and mutable settlement evidence cannot be smuggled into its snapshot.
var providerLineJSON = object(map[string]jsonRule{
	"PriceID": nullable(uuidValue), "ProductID": nullable(uuidValue), "Description": textValue,
	"Quantity": nullable(integerValue), "UnitAmount": integerValue, "Amount": integerValue,
	"ProviderAccessDefault": booleanValue, "Source": textValue, "BillingIntervalHours": nullable(integerValue), "AccessDurationHours": nullable(integerValue),
	"Ownership": textValue, "ClaimKey": textValue, "ProviderReference": textValue,
})

var providerBillShape = object(map[string]jsonRule{
	"CalendarAnchor": nullable(textValue),
	"PSPID":          uuidValue, "CustomerID": uuidValue, "SubscriptionID": nullable(uuidValue), "ExistingOrderID": nullable(uuidValue),
	"Owner": textValue, "Kind": textValue, "SourceKind": textValue, "SourceKey": textValue, "Currency": textValue,
	"Total": integerValue, "PeriodStart": textValue, "PeriodEnd": textValue, "DueAt": textValue,
	"Lines": array(providerLineJSON), "ProviderReference": textValue, "Payment": func(v any) bool { return v == nil },
	"Settled": booleanValue, "SettlementSource": textValue, "NonCashAmount": integerValue,
})

func providerBillJSON(schedule bool) jsonRule {
	return func(v any) bool {
		if !providerBillShape(v) {
			return false
		}
		raw, err := json.Marshal(v)
		if err != nil || len(raw) > 262144 {
			return false
		}
		var bill observed.Bill
		if json.Unmarshal(raw, &bill) != nil || bill.PSPID == uuid.Nil || bill.CustomerID == uuid.Nil || bill.ExistingOrderID != nil || bill.Payment != nil || bill.Settled || bill.SettlementSource != "" || bill.NonCashAmount != 0 || bill.Total < 0 {
			return false
		}
		if bill.CalendarAnchor != nil && (bill.CalendarAnchor.IsZero() || !schedule && bill.CalendarAnchor.After(bill.PeriodStart)) {
			return false
		}
		if bill.Owner != observed.OwnerEngine && bill.Owner != observed.OwnerProvider && bill.Owner != observed.OwnerNMISchedule || moneyutil.RequireFiatCurrency(bill.Currency) != nil || bill.Currency != moneyutil.NormalizeCurrency(bill.Currency) {
			return false
		}
		if schedule {
			if bill.Kind != "renewal" || bill.SourceKind != "provider_cycle" || bill.SourceKey != "" || strings.TrimSpace(bill.ProviderReference) == "" || len(bill.ProviderReference) > 512 || bill.SubscriptionID == nil || *bill.SubscriptionID == uuid.Nil || bill.Owner == observed.OwnerEngine || bill.Total <= 0 || !bill.PeriodStart.IsZero() || !bill.PeriodEnd.IsZero() || !bill.DueAt.IsZero() {
				return false
			}
		} else {
			if strings.TrimSpace(bill.SourceKind) == "" || len(bill.SourceKind) > 128 || strings.TrimSpace(bill.SourceKey) == "" || len(bill.SourceKey) > 512 || len(bill.ProviderReference) > 512 {
				return false
			}
			switch bill.Kind {
			case "purchase":
				if bill.SubscriptionID != nil {
					return false
				}
			case "initial", "renewal", "adjustment":
				if bill.SubscriptionID == nil || *bill.SubscriptionID == uuid.Nil || bill.PeriodStart.IsZero() || !bill.PeriodEnd.After(bill.PeriodStart) {
					return false
				}
			default:
				return false
			}
		}
		if len(bill.Lines) == 0 || len(bill.Lines) > 100 {
			return false
		}
		var total int64
		for _, line := range bill.Lines {
			if strings.TrimSpace(line.Description) == "" || line.UnitAmount < 0 || line.Amount < 0 && line.Source != "provider_adjustment" {
				return false
			}
			for _, n := range []*int{line.Quantity, line.BillingIntervalHours, line.AccessDurationHours} {
				if n != nil && (*n < 1 || *n > math.MaxInt32) {
					return false
				}
			}
			if line.Source == "" && (line.PriceID == nil || *line.PriceID == uuid.Nil || line.ProductID == nil || *line.ProductID == uuid.Nil) || line.Source != "" && (line.PriceID != nil || line.ProductID != nil || line.Ownership != "" || line.ClaimKey != "") {
				return false
			}
			if line.Amount > 0 && total > math.MaxInt64-line.Amount || line.Amount < 0 && total < math.MinInt64-line.Amount {
				return false
			}
			total += line.Amount
		}
		return total == bill.Total
	}
}

func validateProviderBillValues(p Profile, values []*string) error {
	field := "snapshot"
	if p.Name == "provider_schedules" {
		field = "accepted_bill"
	}
	raw := value(p, values, field)
	if raw == nil || len(*raw) > 262144 || validateJSON(p.Name+"."+field, *raw) != nil {
		return fmt.Errorf("invalid %s.%s", p.Name, field)
	}
	var bill observed.Bill
	if err := json.Unmarshal([]byte(*raw), &bill); err != nil {
		return err
	}
	match := func(name, expected string) bool { v := value(p, values, name); return v != nil && *v == expected }
	if !match("psp_id", bill.PSPID.String()) || !match("customer_id", bill.CustomerID.String()) {
		return fmt.Errorf("provider bill snapshot belongs to another identity")
	}
	if p.Name == "provider_bills" {
		if !match("source_kind", bill.SourceKind) || !match("source_key", bill.SourceKey) || !match("provider_reference", bill.ProviderReference) {
			return fmt.Errorf("provider bill snapshot differs from its persisted identity")
		}
	} else {
		if bill.SubscriptionID == nil || !match("subscription_id", bill.SubscriptionID.String()) {
			return fmt.Errorf("provider schedule names another subscription")
		}
		cadence := value(p, values, "cadence_hours")
		if cadence == nil {
			return fmt.Errorf("provider schedule lacks cadence")
		}
		hours, err := strconv.ParseInt(*cadence, 10, 32)
		monthsValue := value(p, values, "cadence_months")
		if monthsValue == nil {
			return fmt.Errorf("provider schedule lacks calendar cadence")
		}
		months, monthErr := strconv.ParseInt(*monthsValue, 10, 32)
		reference := value(p, values, "source_reference")
		providerReference := value(p, values, "provider_reference")
		if providerReference == nil || strings.TrimSpace(*providerReference) == "" || len(*providerReference) > 512 || *providerReference != bill.ProviderReference {
			return fmt.Errorf("provider schedule lacks provider reference")
		}
		version := value(p, values, "accepted_version")
		if version == nil {
			return fmt.Errorf("provider schedule lacks accepted version")
		}
		if n, err := strconv.ParseInt(*version, 10, 64); err != nil || n < 1 {
			return fmt.Errorf("provider schedule has invalid accepted version")
		}
		if err != nil || monthErr != nil || hours < 0 || hours > math.MaxInt64/int64(time.Hour) || hours%24 != 0 || months < 0 || months > 1200 || (hours > 0) == (months > 0) || reference == nil || strings.TrimSpace(*reference) == "" || len(*reference) > 512 || len(bill.Lines) != 1 || hours > 0 && (bill.Lines[0].BillingIntervalHours == nil || int64(*bill.Lines[0].BillingIntervalHours) != hours) || months > 0 && bill.Lines[0].BillingIntervalHours != nil {
			return fmt.Errorf("provider schedule cadence differs from its accepted terms")
		}
		if bill.CalendarAnchor != nil {
			first := value(p, values, "first_due_at")
			if first == nil {
				return fmt.Errorf("provider schedule lacks its effective boundary")
			}
			firstDue, err := time.Parse("2006-01-02 15:04:05.999999-07", *first)
			if err != nil {
				return fmt.Errorf("provider schedule has invalid effective boundary")
			}
			if _, err := observed.CalendarScheduleAnchor(gen.BillingProviderSchedule{FirstDueAt: firstDue, CadenceHours: int32(hours), CadenceMonths: int32(months), AcceptedBill: []byte(*raw)}, firstDue); err != nil {
				return fmt.Errorf("provider schedule calendar anchor contradicts its effective boundary")
			}
		}
	}
	return nil
}
