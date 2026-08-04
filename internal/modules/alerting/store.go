package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// store wraps the sqlc catalog for the alerting tables. Merchant-scoped methods
// resolve to the request/eval merchant's pinned connection (RLS); the armed-
// merchant scan runs cross-merchant on the base pool.
type store struct {
	db *db.DB
}

func newStore(database *db.DB) *store { return &store{db: database} }

// --- rules -------------------------------------------------------------------

type ruleWrite struct {
	name     string
	template string
	params   map[string]any
	severity Severity
	channels []ChannelRef
	enabled  bool
}

func (s *store) createRule(ctx context.Context, w ruleWrite) (Rule, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Rule{}, err
	}
	params, err := marshalJSON(w.params, "{}")
	if err != nil {
		return Rule{}, err
	}
	channels, err := marshalJSON(w.channels, "[]")
	if err != nil {
		return Rule{}, err
	}
	row, err := s.db.Gen(ctx).CreateAlertRule(ctx, gen.CreateAlertRuleParams{
		MerchantID: mid.UUID(),
		Name:       w.name,
		Template:   w.template,
		Params:     params,
		Severity:   string(w.severity),
		Channels:   channels,
		Enabled:    w.enabled,
	})
	if err != nil {
		return Rule{}, err
	}
	return ruleFromRow(row)
}

func (s *store) getRule(ctx context.Context, id uuid.UUID) (Rule, error) {
	row, err := s.db.Gen(ctx).GetAlertRule(ctx, id)
	if err != nil {
		return Rule{}, err
	}
	return ruleFromRow(row)
}

func (s *store) listRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.Gen(ctx).ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	return rulesFromRows(rows)
}

func (s *store) listEnabledRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.Gen(ctx).ListEnabledAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	return rulesFromRows(rows)
}

func (s *store) updateRule(ctx context.Context, id uuid.UUID, w ruleWrite) (Rule, error) {
	params, err := marshalJSON(w.params, "{}")
	if err != nil {
		return Rule{}, err
	}
	channels, err := marshalJSON(w.channels, "[]")
	if err != nil {
		return Rule{}, err
	}
	row, err := s.db.Gen(ctx).UpdateAlertRule(ctx, gen.UpdateAlertRuleParams{
		ID:       id,
		Name:     w.name,
		Params:   params,
		Severity: string(w.severity),
		Channels: channels,
		Enabled:  w.enabled,
	})
	if err != nil {
		return Rule{}, err
	}
	return ruleFromRow(row)
}

func (s *store) deleteRule(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.db.Gen(ctx).DeleteAlertRule(ctx, id)
}

func (s *store) markFired(ctx context.Context, id uuid.UUID, firedAt, evaluatedAt time.Time, value float64, dims map[string]string) error {
	detail, _ := json.Marshal(map[string]any{"dimensions": dims})
	return s.db.Gen(ctx).MarkAlertRuleFired(ctx, gen.MarkAlertRuleFiredParams{
		ID: id, FiredAt: firedAt, EvaluatedAt: evaluatedAt, Value: &value, Detail: detail,
	})
}

func (s *store) markCleared(ctx context.Context, id uuid.UUID, clearedAt, evaluatedAt time.Time, value float64) error {
	return s.db.Gen(ctx).MarkAlertRuleCleared(ctx, gen.MarkAlertRuleClearedParams{
		ID: id, ClearedAt: clearedAt, EvaluatedAt: evaluatedAt, Value: &value,
	})
}

func (s *store) touchEvaluated(ctx context.Context, id uuid.UUID, evaluatedAt time.Time, value float64) error {
	return s.db.Gen(ctx).TouchAlertRuleEvaluated(ctx, gen.TouchAlertRuleEvaluatedParams{
		ID: id, EvaluatedAt: evaluatedAt, Value: &value,
	})
}

// listArmedMerchants returns every merchant with at least one enabled rule.
// CROSS-MERCHANT via migration 0021's SECURITY DEFINER reader (or#861) — a
// base-pool SELECT could not answer this: no GUC means alert_rules' RLS matches
// nothing, so the evaluator had never run. Ids only; each merchant's rules are
// then read under its own merchant scope.
func (s *store) listArmedMerchants(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.GenDirectory().ListArmedAlertMerchants(ctx)
	return derefMerchantIDs(rows), err
}

// derefMerchantIDs drops the pointer indirection sqlc emits for a
// set-returning function's column. The definer bodies select a NOT NULL
// column, so a nil is impossible; it is skipped rather than dereferenced.
func derefMerchantIDs(rows []*uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, id := range rows {
		if id != nil {
			out = append(out, *id)
		}
	}
	return out
}

// --- webhooks ----------------------------------------------------------------

func (s *store) createWebhook(ctx context.Context, name, url string, format WebhookFormat, enabled bool) (Webhook, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Webhook{}, err
	}
	row, err := s.db.Gen(ctx).CreateMerchantWebhook(ctx, gen.CreateMerchantWebhookParams{
		MerchantID: mid.UUID(), Name: name, Url: url, Format: string(format), Enabled: enabled,
	})
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) getWebhook(ctx context.Context, id uuid.UUID) (Webhook, error) {
	row, err := s.db.Gen(ctx).GetMerchantWebhook(ctx, id)
	if err != nil {
		return Webhook{}, err
	}
	return webhookFromRow(row), nil
}

func (s *store) listWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.Gen(ctx).ListMerchantWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookFromRow(row))
	}
	return out, nil
}

func (s *store) deleteWebhook(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.db.Gen(ctx).DeleteMerchantWebhook(ctx, id)
}

// --- notifications -----------------------------------------------------------

func (s *store) createNotification(ctx context.Context, n Notification) (Notification, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Notification{}, err
	}
	data, err := marshalJSON(n.Data, "")
	if err != nil {
		return Notification{}, err
	}
	if len(data) == 0 {
		data = nil
	}
	row, err := s.db.Gen(ctx).CreateMerchantNotification(ctx, gen.CreateMerchantNotificationParams{
		MerchantID: mid.UUID(),
		Severity:   string(n.Severity),
		Title:      n.Title,
		Body:       n.Body,
		Link:       n.Link,
		RuleID:     n.RuleID,
		Data:       data,
	})
	if err != nil {
		return Notification{}, err
	}
	return notificationFromRow(row), nil
}

func (s *store) listNotifications(ctx context.Context, unreadOnly bool, limit int) ([]Notification, error) {
	rows, err := s.db.Gen(ctx).ListMerchantNotifications(ctx, gen.ListMerchantNotificationsParams{
		UnreadOnly: unreadOnly, RowLimit: int32(limit), // #nosec G115 -- only caller passes the hardcoded notificationListLimit const (100); no HTTP param feeds limit
	})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, notificationFromRow(row))
	}
	return out, nil
}

func (s *store) markNotificationRead(ctx context.Context, id uuid.UUID) (int64, error) {
	return s.db.Gen(ctx).MarkMerchantNotificationRead(ctx, id)
}

func (s *store) unreadCount(ctx context.Context) (int64, error) {
	return s.db.Gen(ctx).CountUnreadMerchantNotifications(ctx)
}

// countPaymentMethodsExpiring implements templateEvalDeps for the digest.
func (s *store) countPaymentMethodsExpiring(ctx context.Context, now time.Time, daysAhead int) (int64, error) {
	return s.db.Gen(ctx).CountPaymentMethodsExpiringWithin(ctx, gen.CountPaymentMethodsExpiringWithinParams{
		Now: now, DaysAhead: int32(daysAhead), // #nosec G115 -- days_ahead is validated to [1,120] by paramReader.integer before a rule can persist (templates.go)
	})
}

// --- #787 reconciliation-findings notifications -----------------------------

// markFindingNotified stamps the finding's dedupe linkage after a successful
// (or attempted — fire-and-forget, matching the rule evaluator's posture)
// notification push.
func (s *store) markFindingNotified(ctx context.Context, id uuid.UUID, at time.Time, severity string) error {
	_, err := s.db.Gen(ctx).MarkReconciliationFindingNotified(ctx, gen.MarkReconciliationFindingNotifiedParams{
		ID: id, NotifiedAt: at, Severity: severity,
	})
	return err
}

// listArmedFindingsDigestMerchants: merchants with at least one undigested
// low-severity requires_review finding — the digest sweep's work set.
// CROSS-MERCHANT via 0021's SECURITY DEFINER reader, same as listArmedMerchants
// and for the same reason (or#861).
func (s *store) listArmedFindingsDigestMerchants(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.GenDirectory().ListArmedFindingsDigestMerchants(ctx)
	return derefMerchantIDs(rows), err
}

func (s *store) getFindingDigestWatermark(ctx context.Context) (*time.Time, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).GetFindingDigestWatermark(ctx, mid.UUID())
}

func (s *store) touchFindingDigestWatermark(ctx context.Context, at time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.Gen(ctx).TouchFindingDigestWatermark(ctx, gen.TouchFindingDigestWatermarkParams{
		MerchantID: mid.UUID(), LastDigestedAt: at,
	})
}

func (s *store) countLowSeverityFindingsPendingDigest(ctx context.Context) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.db.Gen(ctx).CountLowSeverityFindingsPendingDigest(ctx, mid.UUID())
}

func (s *store) markLowSeverityFindingsDigested(ctx context.Context, at time.Time) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.db.Gen(ctx).MarkLowSeverityFindingsDigested(ctx, gen.MarkLowSeverityFindingsDigestedParams{
		MerchantID: mid.UUID(), NotifiedAt: at,
	})
}

// webhookExpectedRails implements templateEvalDeps for the #786 webhook_silence
// expectation gate: armed rails with billable subscriptions (rail -> count).
func (s *store) webhookExpectedRails(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.Gen(ctx).ListWebhookExpectedRails(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Rail] = row.Billable
	}
	return out, nil
}

// --- row mapping -------------------------------------------------------------

func ruleFromRow(row gen.OpenrailsAlertRule) (Rule, error) {
	var params map[string]any
	if len(row.Params) > 0 {
		if err := json.Unmarshal(row.Params, &params); err != nil {
			return Rule{}, fmt.Errorf("alerting: decode rule params: %w", err)
		}
	}
	var channels []ChannelRef
	if len(row.Channels) > 0 {
		if err := json.Unmarshal(row.Channels, &channels); err != nil {
			return Rule{}, fmt.Errorf("alerting: decode rule channels: %w", err)
		}
	}
	return Rule{
		ID: row.ID, MerchantID: row.MerchantID, Name: row.Name, Template: row.Template,
		Params: params, Severity: Severity(row.Severity), Channels: channels, Enabled: row.Enabled,
		FiredAt: row.FiredAt, ClearedAt: row.ClearedAt, LastEvaluatedAt: row.LastEvaluatedAt,
		LastValue: row.LastValue, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func rulesFromRows(rows []gen.OpenrailsAlertRule) ([]Rule, error) {
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		r, err := ruleFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func webhookFromRow(row gen.OpenrailsMerchantWebhook) Webhook {
	return Webhook{
		ID: row.ID, MerchantID: row.MerchantID, Name: row.Name, URL: row.Url,
		Format: WebhookFormat(row.Format), Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func notificationFromRow(row gen.OpenrailsMerchantNotification) Notification {
	n := Notification{
		ID: row.ID, Severity: Severity(row.Severity), Title: row.Title, Body: row.Body,
		Link: row.Link, RuleID: row.RuleID, CreatedAt: row.CreatedAt, ReadAt: row.ReadAt,
	}
	if len(row.Data) > 0 {
		var d any
		if err := json.Unmarshal(row.Data, &d); err == nil {
			n.Data = d
		}
	}
	return n
}

// marshalJSON encodes v, substituting empty for a nil/zero value.
func marshalJSON(v any, empty string) ([]byte, error) {
	if v == nil {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("alerting: encode jsonb: %w", err)
	}
	if string(b) == "null" {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	return b, nil
}
