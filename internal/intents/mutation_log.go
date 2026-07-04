package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

type MutationLogPhase string

const (
	MutationLogPhaseAttempting MutationLogPhase = "attempting"
	MutationLogPhaseSucceeded  MutationLogPhase = "succeeded"
	MutationLogPhaseFailed     MutationLogPhase = "failed"
	MutationLogPhaseUnknown    MutationLogPhase = "unknown"
	MutationLogPhaseParked     MutationLogPhase = "parked"
)

type MutationLogParams struct {
	MerchantID            uuid.UUID
	Provider              string
	RailMerchantAccountID *uuid.UUID
	ProviderIntentID      *uuid.UUID
	IntentType            string
	IdempotencyKey        string
	Attempt               int32
	Phase                 MutationLogPhase
	Reason                string
	Evidence              map[string]any
}

type MutationLogger interface {
	LogExternalMutation(ctx context.Context, p MutationLogParams) error
}

var (
	sensitiveMutationKey = regexp.MustCompile(`(?i)(authorization|api[_-]?key|security[_-]?key|secret|password|private[_-]?key|card|pan|cvv|cvc|token)`)
	sensitiveAssignment  = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|security[_-]?key|secret|password|private[_-]?key|card[_-]?number|pan|cvv|cvc|token)\s*[:=]\s*[^,\s&]+`)
)

func (s *Store) LogExternalMutation(ctx context.Context, p MutationLogParams) error {
	if p.MerchantID == uuid.Nil || p.Provider == "" || p.Phase == "" {
		return fmt.Errorf("intents: mutation log requires merchant_id, provider, and phase")
	}
	var reason *string
	if p.Reason != "" {
		r := scrubMutationString(p.Reason)
		reason = &r
	}
	var evidence []byte
	if len(p.Evidence) > 0 {
		b, err := json.Marshal(scrubMutationEvidence(p.Evidence))
		if err != nil {
			return fmt.Errorf("intents: marshal mutation log evidence: %w", err)
		}
		evidence = b
	}
	intentType := emptyStringNil(p.IntentType)
	idempotencyKey := emptyStringNil(p.IdempotencyKey)
	return s.db.Gen(ctx).InsertRailMutationLog(ctx, gen.InsertRailMutationLogParams{
		MerchantID:            p.MerchantID,
		Rail:                  p.Provider,
		RailMerchantAccountID: p.RailMerchantAccountID,
		RailIntentID:          p.ProviderIntentID,
		IntentType:            intentType,
		IdempotencyKey:        idempotencyKey,
		Attempt:               p.Attempt,
		Phase:                 string(p.Phase),
		Reason:                reason,
		Evidence:              evidence,
	})
}

func emptyStringNil(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func scrubMutationEvidence(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = scrubMutationValue(k, v)
	}
	return out
}

func scrubMutationValue(key string, v any) any {
	if sensitiveMutationKey.MatchString(key) {
		return "[redacted]"
	}
	switch x := v.(type) {
	case string:
		return scrubMutationString(x)
	case map[string]any:
		return scrubMutationEvidence(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = scrubMutationValue("", item)
		}
		return out
	default:
		return v
	}
}

func scrubMutationString(s string) string {
	return sensitiveAssignment.ReplaceAllString(s, "$1=[redacted]")
}
