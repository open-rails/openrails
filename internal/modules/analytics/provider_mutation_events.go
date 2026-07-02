package analytics

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type ProviderMutationEventFilter struct {
	MerchantID            string
	Provider              *string
	ProviderIntentID      *uuid.UUID
	RailMerchantAccountID *uuid.UUID
	Phase                 *string
	Limit                 int
}

func (s *EventLogService) CountProviderMutationEvents(ctx context.Context, f ProviderMutationEventFilter) (uint64, error) {
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return 0, fmt.Errorf("clickhouse not configured")
	}
	where, args, err := providerMutationWhere(f)
	if err != nil {
		return 0, err
	}
	if err := s.ensureConn(ctx); err != nil {
		return 0, err
	}
	var total uint64
	if err := s.clickhouseConn.QueryRow(ctx, "SELECT count() FROM provider_mutation_events "+where, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *EventLogService) ListProviderMutationEvents(ctx context.Context, f ProviderMutationEventFilter) ([]ProviderMutationEventData, error) {
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil, fmt.Errorf("clickhouse not configured")
	}
	where, args, err := providerMutationWhere(f)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	args = append(args, limit)
	if err := s.ensureConn(ctx); err != nil {
		return nil, err
	}
	rows, err := s.clickhouseConn.Query(ctx, `
		SELECT event_id, merchant_id, provider, rail_merchant_account_id, provider_intent_id,
		       intent_type, idempotency_key, attempt, phase, reason, evidence, timestamp
		FROM provider_mutation_events
		`+where+`
		ORDER BY timestamp DESC, event_id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderMutationEventData
	for rows.Next() {
		var event ProviderMutationEventData
		if err := rows.Scan(
			&event.EventID,
			&event.MerchantID,
			&event.Provider,
			&event.RailMerchantAccountID,
			&event.ProviderIntentID,
			&event.IntentType,
			&event.IdempotencyKey,
			&event.Attempt,
			&event.Phase,
			&event.Reason,
			&event.Evidence,
			&event.Timestamp,
		); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func providerMutationWhere(f ProviderMutationEventFilter) (string, []any, error) {
	if strings.TrimSpace(f.MerchantID) == "" {
		return "", nil, fmt.Errorf("merchant_id is required")
	}
	clauses := []string{"merchant_id = ?"}
	args := []any{f.MerchantID}
	if f.Provider != nil {
		clauses = append(clauses, "provider = ?")
		args = append(args, *f.Provider)
	}
	if f.ProviderIntentID != nil {
		clauses = append(clauses, "provider_intent_id = ?")
		args = append(args, *f.ProviderIntentID)
	}
	if f.RailMerchantAccountID != nil {
		clauses = append(clauses, "rail_merchant_account_id = ?")
		args = append(args, *f.RailMerchantAccountID)
	}
	if f.Phase != nil {
		clauses = append(clauses, "phase = ?")
		args = append(args, *f.Phase)
	}
	return "WHERE " + strings.Join(clauses, " AND "), args, nil
}
