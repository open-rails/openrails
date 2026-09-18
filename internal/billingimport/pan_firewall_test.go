package billingimport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func declaredBook() DeclaredBilling {
	customer, price, product := uuid.New(), uuid.New(), uuid.New()
	return DeclaredBilling{
		AsOf:       time.Now().UTC(),
		DefaultPSP: PSPRef{Key: "nmi-main"},
		Customers:  []DeclaredCustomer{{Customer: customer, Email: "payer@example.test"}},
		PaymentMethods: []DeclaredPaymentMethod{{
			Customer: customer, Rail: "nmi", RailCustomerRef: uuid.NewString(),
			RailMethodRef: uuid.NewString(), InitialTransactionID: "1234567890",
			LastFour: "1111", CardType: "visa", ExpiryDate: "12/29",
		}},
		Subscriptions: []DeclaredSubscription{{
			SourceID: "sub_" + uuid.NewString(), Customer: customer, Price: price,
			Rail: "nmi", RailSubscriptionID: uuid.NewString(), UserEmail: "payer@example.test",
			Evidence: json.RawMessage(`{"legacy_id":"` + uuid.NewString() + `"}`),
		}},
		Transactions: []DeclaredTransaction{{
			RailSubscriptionID: uuid.NewString(), TransactionID: uuid.NewString(),
			Currency: "USD", OccurredAt: time.Now().UTC(),
		}},
		AdminGrants: []DeclaredAdminGrant{{
			Customer: customer, Product: product, SourceID: "grant_" + uuid.NewString(),
		}},
	}
}

// A declared book of identifiers imports; identifiers are not card numbers,
// whatever their digits happen to spell.
func TestDeclaredBookOfIdentifiersIsNotCardInput(t *testing.T) {
	t.Parallel()
	require.NoError(t, rejectDeclaredPANs(declaredBook()))

	// UUIDs that the old Luhn-only scan read as card numbers.
	for _, handle := range []string{
		"a544fda7-1958-4199-9417-3263a6c4b369",
		"72967ae8-6314-4231-9276-8176f5be4867",
		"805b6084-0619-4770-9a20-90104f447f64",
	} {
		book := declaredBook()
		book.PaymentMethods[0].RailMethodRef = handle
		book.Subscriptions[0].SourceID = "sub_" + handle
		book.Transactions[0].TransactionID = handle
		require.NoErrorf(t, rejectDeclaredPANs(book), "handle %s", handle)
	}
}

// Every declared field a card could be pasted into refuses the whole book.
func TestDeclaredBookRefusesCardNumbers(t *testing.T) {
	t.Parallel()
	const visa = "4111111111111111"
	for name, mutate := range map[string]func(*DeclaredBilling){
		"payment_methods.rail_method_ref":        func(b *DeclaredBilling) { b.PaymentMethods[0].RailMethodRef = visa },
		"payment_methods.rail_customer_ref":      func(b *DeclaredBilling) { b.PaymentMethods[0].RailCustomerRef = visa },
		"payment_methods.initial_transaction_id": func(b *DeclaredBilling) { b.PaymentMethods[0].InitialTransactionID = visa },
		"payment_methods.last_four":              func(b *DeclaredBilling) { b.PaymentMethods[0].LastFour = "4111 1111 1111 1111" },
		"payment_methods.card_type":              func(b *DeclaredBilling) { b.PaymentMethods[0].CardType = "visa 4111-1111-1111-1111" },
		"payment_methods.expiry_date":            func(b *DeclaredBilling) { b.PaymentMethods[0].ExpiryDate = visa },
		"subscriptions.source_id":                func(b *DeclaredBilling) { b.Subscriptions[0].SourceID = visa },
		"subscriptions.rail_subscription_id":     func(b *DeclaredBilling) { b.Subscriptions[0].RailSubscriptionID = visa },
		"subscriptions.user_email":               func(b *DeclaredBilling) { b.Subscriptions[0].UserEmail = visa + "@example.test" },
		"subscriptions.evidence": func(b *DeclaredBilling) {
			b.Subscriptions[0].Evidence = json.RawMessage(`{"card":"` + visa + `"}`)
		},
		"subscriptions.payment_method.rail_method_ref": func(b *DeclaredBilling) {
			b.Subscriptions[0].PaymentMethod = &PaymentMethodRef{Rail: "nmi", RailMethodRef: visa}
		},
		"transactions.transaction_id": func(b *DeclaredBilling) { b.Transactions[0].TransactionID = "3782 822463 10005" },
		"admin_grants.source_id":      func(b *DeclaredBilling) { b.AdminGrants[0].SourceID = "5555-5555-5555-4444" },
		"customers.email":             func(b *DeclaredBilling) { b.Customers[0].Email = visa + "@example.test" },
		"default_psp.key":             func(b *DeclaredBilling) { b.DefaultPSP.Key = visa },
	} {
		book := declaredBook()
		mutate(&book)
		err := rejectDeclaredPANs(book)
		require.Errorf(t, err, "a card number in %s must refuse the book", name)
		require.Containsf(t, err.Error(), "card-number-shaped", "%s", name)
	}
}
