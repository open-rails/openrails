package openrails

import (
	"encoding/json"
	"github.com/google/uuid"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCreditTransactionWireContract(t *testing.T) {
	when, err := time.Parse(time.RFC3339Nano, "2026-09-16T00:00:00.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	balance, zero := int64(math.MinInt64), int64(0)
	value := CreditTransaction{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), CustomerID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Invoker: "host", Currency: "USD", Amount: math.MaxInt64, BalanceAfter: &balance, TransactionType: "deposit", Status: "completed", Captured: &zero, Source: "bank", CreatedAt: when, UpdatedAt: when}
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
	var got CreditTransaction
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
		customer := CustomerID(uuid.New())
		request := DepositCreditsRequest{CustomerID: &customer, Invoker: "host", Currency: "USD", Amount: amount, Source: "bank", SourceID: "payment"}
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var got DepositCreditsRequest
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(request, got) {
			t.Fatalf("deposit changed: %s", raw)
		}
		balance := CreditAccount{CustomerID: customer.String(), Currency: "USD", BalanceAmount: amount, HeldAmount: amount, AvailableAmount: amount, OutstandingOwedAmount: amount}
		raw, err = json.Marshal(balance)
		if err != nil {
			t.Fatal(err)
		}
		var gotBalance CreditAccount
		if err := json.Unmarshal(raw, &gotBalance); err != nil {
			t.Fatal(err)
		}
		if balance != gotBalance {
			t.Fatalf("balance changed: %s", raw)
		}
	}
	var request DepositCreditsRequest
	for _, raw := range []string{`{"amount":9007199254740993}`, `{"amount":"9223372036854775808"}`, `{"customer_id":"invalid"}`} {
		if err := json.Unmarshal([]byte(raw), &request); err == nil {
			t.Fatalf("accepted invalid wire value %s", raw)
		}
	}
}

func TestAdmissionAndUsageMoneyWire(t *testing.T) {
	for _, value := range []any{
		AdmitRequest{EstimatedAmount: 9007199254740993, AccrualRateDeltaPerHour: math.MaxInt64},
		AdmitResponse{Allowed: true, EstimatedAmount: 9007199254740993, StartCapacityAmount: math.MaxInt64},
		UsageReport{Amount: math.MaxInt64},
		WastedSpendReport{Amount: math.MaxInt64},
		WastedSpendResponse{RecordedAmount: math.MaxInt64, ChargedAmount: 9007199254740993},
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
