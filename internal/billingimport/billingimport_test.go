package billingimport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

const visa = "4111111111111111"

func declaredBook() DeclaredBilling {
	customer := openrails.CustomerID(uuid.New())
	return DeclaredBilling{
		AsOf:       time.Now().UTC(),
		DefaultPSP: PSPRef{Key: "nmi-main"},
		Customers:  []DeclaredCustomer{{Customer: customer, Email: "payer@example.test"}},
		PaymentMethods: []DeclaredPaymentMethod{{
			Customer: customer, Rail: "nmi", RailCustomerRef: uuid.NewString(), RailMethodRef: uuid.NewString(),
			InitialTransactionID: "1234567890", LastFour: "1111", CardType: "visa", ExpiryDate: "12/29",
		}},
		Subscriptions: []DeclaredSubscription{{
			SourceID: "sub_" + uuid.NewString(), Customer: customer, Price: openrails.PriceID(uuid.New()),
			Rail: "nmi", RailSubscriptionID: uuid.NewString(), UserEmail: "payer@example.test",
			Evidence: json.RawMessage(`{"legacy_id":"` + uuid.NewString() + `"}`),
		}},
		Transactions: []DeclaredTransaction{{RailSubscriptionID: uuid.NewString(), TransactionID: uuid.NewString(), Currency: "USD", OccurredAt: time.Now().UTC()}},
		AdminGrants:  []DeclaredAdminGrant{{Customer: customer, Product: openrails.ProductID(uuid.New()), SourceID: "grant_" + uuid.NewString()}},
	}
}

// importWithoutDB proves a refusal happens before any database work: Options has no DB.
func importWithoutDB(book DeclaredBilling) error {
	_, err := Import(context.Background(), Options{Book: book})
	return err
}

// SAQ A: identifiers import, whatever their digits spell (UUIDs a Luhn-only scan
// misread); a card pasted into ANY declared column refuses the whole book.
func TestDeclaredBookPANFirewall(t *testing.T) {
	t.Parallel()
	require.NoError(t, rejectDeclaredPANs(declaredBook()))
	for _, handle := range []string{"a544fda7-1958-4199-9417-3263a6c4b369", "72967ae8-6314-4231-9276-8176f5be4867", "805b6084-0619-4770-9a20-90104f447f64"} {
		book := declaredBook()
		book.PaymentMethods[0].RailMethodRef = handle
		book.Subscriptions[0].SourceID = "sub_" + handle
		book.Transactions[0].TransactionID = handle
		require.NoError(t, rejectDeclaredPANs(book), handle)
	}

	for name, mutate := range map[string]func(*DeclaredBilling){
		"pm.rail_method_ref":          func(b *DeclaredBilling) { b.PaymentMethods[0].RailMethodRef = visa },
		"pm.rail_customer_ref":        func(b *DeclaredBilling) { b.PaymentMethods[0].RailCustomerRef = visa },
		"pm.initial_transaction_id":   func(b *DeclaredBilling) { b.PaymentMethods[0].InitialTransactionID = visa },
		"pm.recurring_transaction_id": func(b *DeclaredBilling) { b.PaymentMethods[0].RecurringTransactionID = visa },
		"pm.last_four":                func(b *DeclaredBilling) { b.PaymentMethods[0].LastFour = "4111 1111 1111 1111" },
		"pm.card_type":                func(b *DeclaredBilling) { b.PaymentMethods[0].CardType = "visa 4111-1111-1111-1111" },
		"pm.expiry_date":              func(b *DeclaredBilling) { b.PaymentMethods[0].ExpiryDate = visa },
		"pm.psp.key":                  func(b *DeclaredBilling) { b.PaymentMethods[0].PSP.Key = visa },
		"sub.source_id":               func(b *DeclaredBilling) { b.Subscriptions[0].SourceID = visa },
		"sub.rail_subscription_id":    func(b *DeclaredBilling) { b.Subscriptions[0].RailSubscriptionID = visa },
		"sub.user_email":              func(b *DeclaredBilling) { b.Subscriptions[0].UserEmail = visa + "@example.test" },
		"sub.cancel.kind":             func(b *DeclaredBilling) { b.Subscriptions[0].Cancel.Kind = visa },
		"sub.evidence":                func(b *DeclaredBilling) { b.Subscriptions[0].Evidence = json.RawMessage(`{"card":"` + visa + `"}`) },
		"sub.payment_method": func(b *DeclaredBilling) {
			b.Subscriptions[0].PaymentMethod = &PaymentMethodRef{Rail: "nmi", RailMethodRef: visa}
		},
		"txn.transaction_id": func(b *DeclaredBilling) { b.Transactions[0].TransactionID = "3782 822463 10005" },
		"grant.source_id":    func(b *DeclaredBilling) { b.AdminGrants[0].SourceID = "5555-5555-5555-4444" },
		"customer.email":     func(b *DeclaredBilling) { b.Customers[0].Email = visa + "@example.test" },
		"default_psp.key":    func(b *DeclaredBilling) { b.DefaultPSP.Key = visa },
	} {
		book := declaredBook()
		mutate(&book)
		err := importWithoutDB(book)
		require.ErrorIs(t, err, ErrInvalidDeclaredInput, name)
		require.ErrorContains(t, err, "card-number-shaped", name)
		require.NotContains(t, err.Error(), visa, "the refusal must not echo the card")
	}
}

// PostgreSQL normalizes JSONB (unescapes strings, expands exponents), so evidence
// is scanned as decoded tokens — keys, duplicates and whole-number spellings too.
func TestDeclaredEvidenceScansDecodedJSON(t *testing.T) {
	for _, raw := range []string{
		`{"card":"4111111111111111"}`,
		`{"nested":[{"4111111111111111":"value"}]}`,
		`{"card":"4111111111111111","card":"overwritten"}`,
		`{"card":4111111111111111}`,
		`{"card":-4111111111111111}`,
		`{"card":4.111111111111111e15}`,
		`{"card":411111111111111100e-2}`,
		`{"card":4111111111111111.0}`,
	} {
		book := declaredBook()
		book.Subscriptions[0].Evidence = json.RawMessage(raw)
		require.ErrorContains(t, importWithoutDB(book), "card-number-shaped", raw)
	}
	for _, raw := range []string{
		`{"nested":[{"id":"a544fda7-1958-4199-9417-3263a6c4b369"}]}`, `null`, `{"as_of":1770000000000000}`,
		`{"huge":1e10000000}`, `{"tiny":1e-10000000}`, `{"fraction":4.111111111111111}`, `{"huge":1e999999999999999999999}`,
	} {
		book := declaredBook()
		book.Subscriptions[0].Evidence = json.RawMessage(raw)
		require.NoError(t, rejectDeclaredPANs(book), raw)
	}
	for _, raw := range []string{`{"value":`, `{} {}`, `"` + visa} {
		book := declaredBook()
		book.Subscriptions[0].Evidence = json.RawMessage(raw)
		err := importWithoutDB(book)
		require.ErrorIs(t, err, ErrInvalidDeclaredInput, raw)
		require.NotContains(t, err.Error(), visa)
	}
}

func TestWholeJSONNumber(t *testing.T) {
	for in, want := range map[string]string{
		"4111111111111111":     visa,
		"4.111111111111111e15": visa,
		"1e12":                 "1000000000000", // 13 digits: shortest PAN length
		"1e11":                 "",
		"1e19":                 "",
		"12.5":                 "",
		"0.0":                  "",
		"1e99999999999":        "",
	} {
		require.Equal(t, want, wholeJSONNumber(in), in)
	}
}

func testResolver(fallback PSPRef, rows ...gen.OpenrailsPsp) *pspResolver {
	r := &pspResolver{byID: map[uuid.UUID]gen.OpenrailsPsp{}, byKey: map[string]gen.OpenrailsPsp{}, fallback: fallback}
	for _, p := range rows {
		r.byID[p.ID] = p
		r.byKey[pspKeyIndex(p.Rail, *p.Key)] = p
		r.known = append(r.known, p.Rail+"/"+*p.Key)
	}
	return r
}

func psp(rail, key string) gen.OpenrailsPsp {
	return gen.OpenrailsPsp{ID: uuid.New(), Rail: rail, Key: &key}
}

// or#893: every provider row is attributed to a PSP the merchant owns on the same
// rail — per-row ref, else the declared book default, else a refusal (never inferred).
func TestPSPResolution(t *testing.T) {
	mobius, paykings, stripe := psp("nmi", "mobius"), psp("nmi", "paykings"), psp("stripe", "main")
	foreign, nilID := uuid.New(), uuid.Nil
	for _, tc := range []struct {
		name     string
		fallback PSPRef
		ref      PSPRef
		rail     string
		want     uuid.UUID
		errHas   []string
	}{
		{name: "book default", fallback: PSPRef{Key: "mobius"}, rail: "nmi", want: mobius.ID},
		{name: "per-row key wins", fallback: PSPRef{Key: "mobius"}, ref: PSPRef{Key: " PayKings "}, rail: "NMI", want: paykings.ID},
		{name: "per-row id wins", fallback: PSPRef{Key: "mobius"}, ref: PSPRef{ID: &paykings.ID}, rail: "nmi", want: paykings.ID},
		{name: "nil id is no ref", fallback: PSPRef{Key: "mobius"}, ref: PSPRef{ID: &nilID}, rail: "nmi", want: mobius.ID},
		{name: "unattributed", rail: "nmi", errHas: []string{"subscription legacy-1", "default_psp", "nmi/mobius"}},
		{name: "foreign id", ref: PSPRef{ID: &foreign}, rail: "nmi", errHas: []string{"does not own"}},
		{name: "unknown key", ref: PSPRef{Key: "nope"}, rail: "nmi", errHas: []string{"does not own"}},
		{name: "cross-rail id", ref: PSPRef{ID: &stripe.ID}, rail: "nmi", errHas: []string{"rail"}},
		{name: "cross-rail key", ref: PSPRef{Key: "main"}, rail: "nmi", errHas: []string{"does not own"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testResolver(tc.fallback, mobius, paykings, stripe).resolve(tc.ref, tc.rail, "subscription legacy-1")
			if tc.errHas == nil {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
				return
			}
			require.ErrorIs(t, err, ErrInvalidPSPReference)
			require.Equal(t, uuid.Nil, got)
			for _, s := range tc.errHas {
				require.ErrorContains(t, err, s)
			}
		})
	}
}
