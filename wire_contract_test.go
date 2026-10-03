package openrails

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

func TestCreditTransactionWireContract(t *testing.T) {
	when, err := time.Parse(time.RFC3339Nano, "2026-09-16T00:00:00.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	balance, zero := int64(math.MinInt64), int64(0)
	value := billing.CreditTransaction{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), CustomerID: "22222222-2222-2222-2222-222222222222", Invoker: "host", Currency: "USD", Amount: math.MaxInt64, BalanceAfter: &balance, TransactionType: "deposit", Status: "completed", Captured: &zero, Source: "bank", CreatedAt: when, UpdatedAt: when}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/wire/credit_transaction.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("receipt wire changed:\n%s", raw)
	}
	var got billing.CreditTransaction
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, got) {
		t.Fatalf("receipt lost precision or null/time semantics: %#v", got)
	}
	// The ordinary JSON/JavaScript representation is a string, so consumers do
	// not need a custom JSON parser to avoid the IEEE-754 integer boundary.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["amount"] != "9223372036854775807" || decoded["balance_after"] != "-9223372036854775808" {
		t.Fatal(decoded)
	}
}

func TestDepositAndBalanceInt64RoundTrip(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, -1, 0, 1, 9007199254740993, math.MaxInt64} {
		customer := uuid.NewString()
		request := billing.DepositCreditsRequest{CustomerID: &customer, Invoker: "host", Currency: "USD", Amount: amount, Source: "bank", SourceID: "payment"}
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var got billing.DepositCreditsRequest
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(request, got) {
			t.Fatalf("deposit changed: %s", raw)
		}
		balance := billing.CreditAccount{CustomerID: customer, Currency: "USD", BalanceAmount: amount, HeldAmount: amount, AvailableAmount: amount, OutstandingOwedAmount: amount}
		raw, err = json.Marshal(balance)
		if err != nil {
			t.Fatal(err)
		}
		var gotBalance billing.CreditAccount
		if err := json.Unmarshal(raw, &gotBalance); err != nil {
			t.Fatal(err)
		}
		if balance != gotBalance {
			t.Fatalf("balance changed: %s", raw)
		}
	}
	var request billing.DepositCreditsRequest
	for _, raw := range []string{`{"amount":9007199254740993}`, `{"amount":"9223372036854775808"}`, `{"customer_id":123}`} {
		if err := json.Unmarshal([]byte(raw), &request); err == nil {
			t.Fatalf("accepted invalid wire value %s", raw)
		}
	}
}

func TestAdmissionAndUsageMoneyWire(t *testing.T) {
	for _, value := range []any{
		billing.AdmitRequest{EstimatedAmount: 9007199254740993, AccrualRateDeltaPerHour: math.MaxInt64},
		billing.AdmitResponse{Allowed: true, EstimatedAmount: 9007199254740993, StartCapacityAmount: math.MaxInt64},
		billing.UsageReport{Amount: math.MaxInt64},
		billing.WastedSpendReport{Amount: math.MaxInt64},
		billing.WastedSpendResponse{RecordedAmount: math.MaxInt64, ChargedAmount: 9007199254740993},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		target := reflect.New(reflect.TypeOf(value))
		if err := json.Unmarshal(raw, target.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, target.Elem().Interface()) {
			t.Fatalf("wire lost precision: %s", raw)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for name, field := range fields {
			if strings.Contains(name, "amount") || name == "accrual_rate_delta_per_hour" {
				if _, ok := field.(string); !ok {
					t.Fatalf("unsafe monetary number %s: %s", name, raw)
				}
			}
		}
	}
}

func TestPolicyMoneyAndUsageSummaryAreExact(t *testing.T) {
	max := int64(math.MaxInt64)
	for _, value := range []any{
		billing.BudgetWindowInput{Key: "day", WindowSeconds: 86400, Limit: max, Currency: "USD"},
		billing.SpendDelegationInput{Scope: "invoker", ScopeKey: "worker", Windows: []billing.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: max, Currency: "USD"}}},
		billing.BillingPolicyInput{Name: "credit-line", Kind: "outstanding_cap", OutstandingCapAmount: max, AccrualRateCapPerHour: max, CollectionThresholdAmount: &max, DelinquencyAmountFloor: &max},
		billing.MerchantSettings{InvoiceCollectionThreshold: &max, InvoiceMonthlyFloor: &max, ArrearsDelinquencyFloor: &max},
		billing.CreditLimitRequest{CustomerID: (billing.CustomerID(uuid.New())).String(), Currency: "USD", CreditLimitAmount: max},
		billing.UsageRollupRow{Key: "api", EventCount: 1, TotalAmount: max, Currency: "USD"},
		billing.ResourceRevenueResponse{Currency: "USD", RevenueAmount: max, Daily: []billing.ResourceRevenueDailyRow{{Date: "2026-09-16", Currency: "USD", Amount: max}}},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		target := reflect.New(reflect.TypeOf(value))
		if err := json.Unmarshal(raw, target.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, target.Elem().Interface()) {
			t.Fatalf("monetary contract lost precision: %s", raw)
		}
		if !strings.Contains(string(raw), `"9223372036854775807"`) {
			t.Fatalf("browser cannot preserve money exactly: %s", raw)
		}
	}
}
