package reconcile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestEvidenceCurrencyPreservesUnknown(t *testing.T) {
	require.Empty(t, evidenceCurrency(""))
	require.Equal(t, "UNK", evidenceCurrencyValue(""))
	require.Equal(t, "USD", evidenceCurrency(" usd "))
	require.Equal(t, "USD", transactionCurrency(RemoteTransaction{Raw: json.RawMessage(`{"currency":" usd "}`)}))
}

func TestEvidenceRawKeepsProviderPayloadsQueryable(t *testing.T) {
	require.JSONEq(t, `{}`, string(evidenceRaw(nil)))
	require.JSONEq(t, `{"source":"nmi"}`, string(evidenceRaw(json.RawMessage(`{"source":"nmi"}`))))
	require.JSONEq(t, `{"_raw_value":"provider-result"}`, string(evidenceRaw(json.RawMessage(`"provider-result"`))))
	require.JSONEq(t, `{"_raw_text":"<broken>"}`, string(evidenceRaw(json.RawMessage(`<broken>`))))
}

func TestTransactionEventKeyDeduplicatesExactActionButSeparatesActions(t *testing.T) {
	when := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	base := RemoteTransaction{
		TransactionID: "txn-1",
		Source:        "api",
		Type:          TransactionTypeSale,
		Success:       true,
		AmountCents:   999,
		Currency:      "USD",
		OccurredAt:    when,
	}
	enriched := base
	enriched.Email = "updated@example.test"
	enriched.CustomerID = "newly-linked-customer"
	enriched.OrderID = "newly-linked-order"
	enriched.SubscriptionID = "newly-linked-subscription"
	require.Equal(t, transactionEventKey(base), transactionEventKey(enriched), "identity enrichment does not create a second charge")

	otherAction := base
	otherAction.Source = "recurring"
	otherAction.OccurredAt = when.Add(24 * time.Hour)
	require.NotEqual(t, transactionEventKey(base), transactionEventKey(otherAction), "same transaction id may contain distinct provider actions")

	otherAttempt := base
	otherAttempt.Success = false
	otherAttempt.DeclineCode = "05"
	require.NotEqual(t, transactionEventKey(base), transactionEventKey(otherAttempt), "a retry outcome must remain a distinct action")
	otherDecline := otherAttempt
	otherDecline.DeclineCode = "51"
	require.NotEqual(t, transactionEventKey(otherAttempt), transactionEventKey(otherDecline), "distinct provider decline actions must remain separate")

	caseOnly := base
	caseOnly.Currency = " usd "
	require.Equal(t, transactionEventKey(base), transactionEventKey(caseOnly), "currency spelling is canonicalized")
}

func TestTransactionIdentityExtractsNMIHintsFromRaw(t *testing.T) {
	txn := RemoteTransaction{Raw: json.RawMessage(`{"source":"recurring","customer_vault_id":"vault-1","email":"buyer@example.test","order_id":"order-1"}`)}
	source, customer, email, order := transactionIdentity(txn)
	require.Equal(t, "recurring", source)
	require.Equal(t, "vault-1", customer)
	require.Equal(t, "buyer@example.test", email)
	require.Equal(t, "order-1", order)
}

func TestPGEvidenceStoreRejectsMismatchedBinding(t *testing.T) {
	store := &PGEvidenceStore{DB: &db.DB{}}
	fetched := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	binding := PSPBinding{ID: uuid.New(), Rail: "nmi"}
	snap := &RemoteSnapshot{Provider: ProviderStripe, PspID: binding.ID.String(), FetchedAt: fetched}
	err := store.StoreEvidence(merchant.WithID(t.Context(), merchant.ID(uuid.New())), uuid.New(), binding, snap, fetched.Add(-time.Hour), fetched)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match PSP rail")
}

func TestPGEvidenceStoreRejectsUnknownTransactionTime(t *testing.T) {
	store := &PGEvidenceStore{DB: &db.DB{}}
	fetched := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	binding := PSPBinding{ID: uuid.New(), Rail: "nmi"}
	snap := &RemoteSnapshot{
		Provider:  ProviderNMI,
		PspID:     binding.ID.String(),
		FetchedAt: fetched,
		Transactions: []RemoteTransaction{{
			TransactionID: "without-time",
			Type:          TransactionTypeSale,
			Success:       true,
		}},
	}
	err := store.StoreEvidence(merchant.WithID(t.Context(), merchant.ID(uuid.New())), uuid.New(), binding, snap, fetched.Add(-time.Hour), fetched)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no occurred_at")
}
