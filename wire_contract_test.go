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

func TestBalanceTransactionWireContract(t *testing.T) {
	when, err := time.Parse(time.RFC3339Nano, "2026-09-16T00:00:00.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	invoker, grant := "host", billing.CreditGrantID(uuid.MustParse("44444444-4444-4444-8444-444444444444"))
	value := billing.BalanceTransaction{ID: billing.BalanceTransactionID(uuid.MustParse("11111111-1111-1111-1111-111111111111")),
		CustomerID: billing.CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222")), Currency: "USD", Type: billing.BalanceTransactionDeposit,
		Amount: math.MaxInt64, CreditGrantID: &grant, Invoker: &invoker, Source: "bank", SourceID: "deposit-1", CreatedAt: when}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/wire/balance_transaction.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("balance transaction wire changed:\n%s", raw)
	}
	var got billing.BalanceTransaction
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, got) {
		t.Fatalf("balance transaction lost precision or null/time semantics: %#v", got)
	}
	// The ordinary JSON/JavaScript representation is a string, so consumers do
	// not need a custom JSON parser to avoid the IEEE-754 integer boundary.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["amount"] != "9223372036854775807" || decoded["resource"] != nil {
		t.Fatal(decoded)
	}
}

func TestCreditGrantAndBalanceInt64RoundTrip(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -9007199254740993, -1, 0, 1, 9007199254740993, math.MaxInt64} {
		customer := billing.CustomerID(uuid.New())
		request := billing.CreateCreditGrantParams{Invoker: "host", Currency: "USD", Amount: amount, Source: "bank", SourceID: "payment"}
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var got billing.CreateCreditGrantParams
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(request, got) {
			t.Fatalf("credit grant changed: %s", raw)
		}
		balance := billing.Balance{CustomerID: customer, Currency: "USD", BalanceAmount: amount, HeldAmount: amount, AvailableAmount: amount, OwedAmount: amount}
		raw, err = json.Marshal(balance)
		if err != nil {
			t.Fatal(err)
		}
		var gotBalance billing.Balance
		if err := json.Unmarshal(raw, &gotBalance); err != nil {
			t.Fatal(err)
		}
		if balance != gotBalance {
			t.Fatalf("balance changed: %s", raw)
		}
	}
	var request billing.CreateCreditGrantParams
	for _, raw := range []string{`{"amount":9007199254740993}`, `{"amount":"9223372036854775808"}`, `{"currency":123}`} {
		if err := json.Unmarshal([]byte(raw), &request); err == nil {
			t.Fatalf("accepted invalid wire value %s", raw)
		}
	}
}

func TestAdmissionAndUsageMoneyWire(t *testing.T) {
	max := int64(math.MaxInt64)
	for _, value := range []any{
		billing.AdmitParams{EstimatedAmount: 9007199254740993, AccrualRateDeltaPerHour: math.MaxInt64},
		billing.Admission{Allowed: true, EstimatedAmount: 9007199254740993, StartCapacityAmount: math.MaxInt64, CapturedAmount: &max},
		billing.RecordUsageParams{Amount: math.MaxInt64, Outcome: billing.UsageFailed},
		billing.UsageEvent{Amount: 9007199254740993, ForgivenAmount: math.MaxInt64, Outcome: billing.UsageFailed},
		billing.CaptureAdmissionParams{Amount: math.MaxInt64},
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
			if (strings.Contains(name, "amount") || name == "accrual_rate_delta_per_hour") && field != nil {
				if _, ok := field.(string); !ok {
					t.Fatalf("unsafe monetary number %s: %s", name, raw)
				}
			}
		}
	}
	// A capture that names no amount is refused, never read as free.
	var capture billing.CaptureAdmissionParams
	if err := json.Unmarshal([]byte(`{"usage":{"event_type":"inference"}}`), &capture); err == nil {
		t.Fatal("capture without an amount decoded")
	}
}

func TestPolicyMoneyAndUsageSummaryAreExact(t *testing.T) {
	max := int64(math.MaxInt64)
	for _, value := range []any{
		billing.BudgetWindow{Key: "day", WindowSeconds: 86400, Limit: max, Currency: "USD"},
		billing.BillingPolicy{Name: "credit-line", Kind: "outstanding_cap", OutstandingCapAmount: max, AccrualRateCapPerHour: max, CollectionThresholdAmount: &max, DelinquencyAmountFloor: &max},
		billing.MerchantSettings{InvoiceCollectionThreshold: &max, InvoiceMonthlyFloor: &max, ArrearsDelinquencyFloor: &max},
		billing.CustomerSettings{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: max}}, TrustLevels: []billing.TrustLevel{}},
		billing.UsageRow{Key: "api", EventCount: 1, Amount: max},
		billing.CreditGrant{ID: billing.CreditGrantID(uuid.New()), Amount: max, RemainingAmount: max, State: billing.CreditGrantActive},
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
