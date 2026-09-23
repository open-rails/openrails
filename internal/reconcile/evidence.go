package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// EvidenceStore retains the normalized provider snapshot produced by a
// read-only pull. It is separate from Store because evidence is useful to
// analysis even when no local mirror mutation was requested.
type EvidenceStore interface {
	StoreEvidence(ctx context.Context, runID uuid.UUID, binding PSPBinding, snap *RemoteSnapshot, since, until time.Time) error
}

// PGEvidenceStore persists provider-neutral evidence in OpenRails-owned
// analysis tables. Provider rows are upserted by stable event/record keys, so
// rerunning a pull does not duplicate transaction counts.
type PGEvidenceStore struct {
	DB *db.DB
}

var _ EvidenceStore = (*PGEvidenceStore)(nil)

func (s *PGEvidenceStore) StoreEvidence(ctx context.Context, runID uuid.UUID, binding PSPBinding, snap *RemoteSnapshot, since, until time.Time) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("reconcile evidence: database is nil")
	}
	if runID == uuid.Nil {
		return fmt.Errorf("reconcile evidence: run id is empty")
	}
	if snap == nil {
		return fmt.Errorf("reconcile evidence: snapshot is nil")
	}
	if binding.ID == uuid.Nil {
		return fmt.Errorf("reconcile evidence: PSP binding is empty")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	provider := strings.TrimSpace(string(snap.Provider))
	if provider == "" {
		return fmt.Errorf("reconcile evidence: provider is empty")
	}
	if rail := strings.TrimSpace(binding.Rail); rail != "" && !strings.EqualFold(rail, provider) {
		return fmt.Errorf("reconcile evidence: provider %q does not match PSP rail %q", provider, rail)
	}
	if pspID := strings.TrimSpace(snap.PspID); pspID != "" {
		parsed, err := uuid.Parse(pspID)
		if err != nil {
			return fmt.Errorf("reconcile evidence: snapshot PSP id %q is invalid: %w", pspID, err)
		}
		if parsed != binding.ID {
			return fmt.Errorf("reconcile evidence: snapshot PSP %s does not match binding %s", parsed, binding.ID)
		}
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return fmt.Errorf("reconcile evidence: window end precedes start")
	}
	fetchedAt := snap.FetchedAt.UTC()
	if fetchedAt.IsZero() {
		return fmt.Errorf("reconcile evidence: snapshot fetched_at is empty")
	}
	capabilities, err := json.Marshal(snap.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal provider capabilities: %w", err)
	}
	coverage, err := json.Marshal(snap.Coverage)
	if err != nil {
		return fmt.Errorf("marshal provider coverage: %w", err)
	}
	for _, txn := range snap.Transactions {
		if txn.OccurredAt.IsZero() {
			return fmt.Errorf("reconcile evidence: transaction %q has no occurred_at", strings.TrimSpace(txn.TransactionID))
		}
	}
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.CreateProviderEvidenceSnapshot(ctx, gen.CreateProviderEvidenceSnapshotParams{
			MerchantID:          mid.UUID(),
			ReconciliationRunID: runID,
			Provider:            provider,
			PspID:               binding.ID,
			FetchedAt:           fetchedAt,
			WindowSince:         timePtr(since),
			WindowUntil:         timePtr(until),
			Capabilities:        capabilities,
			Coverage:            coverage,
		})
		if err != nil {
			return fmt.Errorf("create provider evidence snapshot: %w", err)
		}
		snapshotID := row.ID
		for _, txn := range snap.Transactions {
			occurredAt := txn.OccurredAt.UTC()
			source, customerRef, email, orderRef := transactionIdentity(txn)
			if _, err := q.UpsertProviderEvidenceTransaction(ctx, gen.UpsertProviderEvidenceTransactionParams{
				MerchantID:      mid.UUID(),
				PspID:           binding.ID,
				Provider:        provider,
				EventKey:        transactionEventKey(txn),
				TransactionID:   strings.TrimSpace(txn.TransactionID),
				SubscriptionRef: strings.TrimSpace(txn.SubscriptionID),
				Type:            strings.TrimSpace(string(txn.Type)),
				Success:         txn.Success,
				AmountCents:     txn.AmountCents,
				Currency:        evidenceCurrencyPtr(transactionCurrency(txn)),
				OccurredAt:      occurredAt,
				Source:          source,
				CustomerRef:     customerRef,
				CustomerEmail:   email,
				OrderRef:        orderRef,
				DeclineCode:     strings.TrimSpace(txn.DeclineCode),
				DeclineReason:   strings.TrimSpace(txn.DeclineReason),
				Raw:             evidenceRaw(txn.Raw),
				SnapshotID:      snapshotID,
				ObservedAt:      fetchedAt,
			}); err != nil {
				return fmt.Errorf("store provider transaction %s: %w", txn.TransactionID, err)
			}
		}
		for _, sub := range snap.Subscriptions {
			if _, err := q.UpsertProviderEvidenceSubscription(ctx, gen.UpsertProviderEvidenceSubscriptionParams{
				SnapshotID:              snapshotID,
				MerchantID:              mid.UUID(),
				PspID:                   binding.ID,
				Provider:                provider,
				RecordKey:               evidenceRecordKey(sub.Raw, sub.RailSubscriptionID),
				ProviderSubscriptionRef: strings.TrimSpace(sub.RailSubscriptionID),
				Status:                  strings.TrimSpace(string(sub.Status)),
				RawStatus:               strings.TrimSpace(sub.RawStatus),
				CustomerRef:             strings.TrimSpace(sub.CustomerID),
				CustomerEmail:           strings.TrimSpace(sub.Email),
				Username:                strings.TrimSpace(sub.Username),
				PlanRef:                 strings.TrimSpace(sub.PlanID),
				NextBillingAt:           sub.NextBillingAt,
				LastBilledAt:            sub.LastBilledAt,
				AmountCents:             sub.AmountCents,
				Currency:                evidenceCurrencyPtr(sub.Currency),
				Raw:                     evidenceRaw(sub.Raw),
			}); err != nil {
				return fmt.Errorf("store provider subscription %s: %w", sub.RailSubscriptionID, err)
			}
		}
		for _, method := range snap.PaymentMethods {
			if _, err := q.UpsertProviderEvidencePaymentMethod(ctx, gen.UpsertProviderEvidencePaymentMethodParams{
				SnapshotID:    snapshotID,
				MerchantID:    mid.UUID(),
				PspID:         binding.ID,
				Provider:      provider,
				RecordKey:     evidenceRecordKey(method.Raw, method.RailCustomerRef),
				CustomerRef:   strings.TrimSpace(method.RailCustomerRef),
				CardLast4:     strings.TrimSpace(method.CardLast4),
				CardExpiry:    strings.TrimSpace(method.CardExpiry),
				CustomerEmail: strings.TrimSpace(method.Email),
				Raw:           evidenceRaw(method.Raw),
			}); err != nil {
				return fmt.Errorf("store provider payment method %s: %w", method.RailCustomerRef, err)
			}
		}
		return nil
	})
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func evidenceCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value
}

func evidenceCurrencyPtr(value string) *string {
	value = evidenceCurrency(value)
	if value == "" {
		return nil
	}
	return &value
}

// evidenceRaw keeps malformed/empty provider payloads queryable without
// making a successful pull fail solely because a provider omitted raw data.
func evidenceRaw(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte(`{}`)
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		b, _ := json.Marshal(map[string]string{"_raw_text": string(raw)})
		return b
	}
	// Evidence tables intentionally constrain raw payloads to objects/arrays so
	// report queries can safely inspect provider fields. Keep scalar provider
	// responses queryable without discarding their value.
	switch value.(type) {
	case map[string]any, []any:
		return trimmed
	default:
		b, _ := json.Marshal(map[string]any{"_raw_value": value})
		return b
	}
}

func evidenceObject(raw json.RawMessage) map[string]any {
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		return object
	}
	return nil
}

func evidenceString(object map[string]any, key string) string {
	if object == nil {
		return ""
	}
	value, _ := object[key].(string)
	return strings.TrimSpace(value)
}

func transactionIdentity(tx RemoteTransaction) (source, customerRef, email, orderRef string) {
	object := evidenceObject(tx.Raw)
	source = strings.ToLower(strings.TrimSpace(tx.Source))
	customerRef = strings.TrimSpace(tx.CustomerID)
	email = strings.ToLower(strings.TrimSpace(tx.Email))
	orderRef = strings.TrimSpace(tx.OrderID)
	if source == "" {
		if action, ok := object["action"].(map[string]any); ok {
			source = strings.ToLower(evidenceString(action, "source"))
		}
		if source == "" {
			source = strings.ToLower(evidenceString(object, "source"))
		}
	}
	if customerRef == "" {
		customerRef = evidenceString(object, "customer_id")
		if customerRef == "" {
			customerRef = evidenceString(object, "customer_vault_id")
		}
		if customerRef == "" {
			customerRef = evidenceString(object, "customerid")
		}
	}
	if email == "" {
		email = strings.ToLower(evidenceString(object, "email"))
	}
	if orderRef == "" {
		orderRef = evidenceString(object, "order_id")
	}
	return source, customerRef, email, orderRef
}

func transactionCurrency(tx RemoteTransaction) string {
	value := evidenceCurrency(tx.Currency)
	if value != "" {
		return value
	}
	return evidenceCurrency(evidenceString(evidenceObject(tx.Raw), "currency"))
}

func transactionEventKey(tx RemoteTransaction) string {
	source, _, _, _ := transactionIdentity(tx)
	// Contact and subscription hints may be enriched on a later pull. They are
	// useful report joins, but must not turn the same provider action into a new
	// charge. Its transaction id and action facts are the stable identity.
	identity := struct {
		TransactionID string
		OccurredAt    string
		Source        string
		Type          TransactionType
		Success       bool
		AmountCents   int64
		Currency      string
		DeclineCode   string
	}{
		TransactionID: strings.TrimSpace(tx.TransactionID),
		OccurredAt:    tx.OccurredAt.UTC().Format(time.RFC3339Nano),
		Source:        source,
		Type:          TransactionType(strings.ToLower(strings.TrimSpace(string(tx.Type)))),
		Success:       tx.Success,
		AmountCents:   tx.AmountCents,
		Currency:      transactionCurrency(tx),
		DeclineCode:   strings.TrimSpace(tx.DeclineCode),
	}
	b, _ := json.Marshal(identity)
	if identity.TransactionID == "" {
		b = append(b, evidenceRaw(tx.Raw)...)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func evidenceRecordKey(raw json.RawMessage, ref string) string {
	if ref = strings.TrimSpace(ref); ref != "" {
		return ref
	}
	h := sha256.Sum256(evidenceRaw(raw))
	return hex.EncodeToString(h[:])
}
