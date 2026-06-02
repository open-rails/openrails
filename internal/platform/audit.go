// Package platform implements the managed-hosting PLATFORM superadmin layer
// (issue #226): cross-tenant audit logging, time-boxed break-glass elevation,
// and platform-wide metrics aggregation. It is DISTINCT from per-tenant operator
// admin (#224): everything here is cross-tenant control-plane state, gated by the
// openrails:platform:superadmin permission held in a separate platform org.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/tenant"
)

// AuditRow is a read view of a billing.platform_audit row (list/inspect).
type AuditRow struct {
	ID             string    `json:"id"`
	ActorUserID    string    `json:"actor_user_id"`
	ActorOrg       string    `json:"actor_org,omitempty"`
	Action         string    `json:"action"`
	TargetTenantID string    `json:"target_tenant_id,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Cross-tenant platform action names recorded in billing.platform_audit. Kept as
// constants so callers and tests agree on the vocabulary.
const (
	ActionTenantList       = "platform.tenant.list"
	ActionTenantInspect    = "platform.tenant.inspect"
	ActionTenantSearch     = "platform.tenant.search"
	ActionTenantProvision  = "platform.tenant.provision"
	ActionTenantSuspend    = "platform.tenant.suspend"
	ActionTenantResume     = "platform.tenant.resume"
	ActionTenantTierChange = "platform.tenant.tier_change"
	ActionTenantDelete     = "platform.tenant.delete"
	ActionMetricsRead      = "platform.metrics.read"
	ActionBreakGlassGrant  = "platform.break_glass.grant"
	ActionBreakGlassRevoke = "platform.break_glass.revoke"
)

// AuditEntry is a single cross-tenant platform audit record.
type AuditEntry struct {
	ActorUserID    string
	ActorOrg       string
	Action         string
	TargetTenantID *tenant.ID // nil for platform-wide actions (list/metrics)
	Reason         string
	Before         any // JSON-serialized into before_state where applicable
	After          any // JSON-serialized into after_state where applicable
	Detail         any // JSON-serialized into detail (extra context)
}

// AuditLog is the append-only cross-tenant platform audit log (issue #226). It
// writes to billing.platform_audit, which lives OUTSIDE tenant-scoped data so a
// record of a superadmin action survives the target tenant's deletion.
type AuditLog struct {
	pool *pgxpool.Pool
}

// NewAuditLog builds the audit log over the control-plane pool (required).
func NewAuditLog(pool *pgxpool.Pool) (*AuditLog, error) {
	if pool == nil {
		return nil, errors.New("platform: audit log requires a pgx pool")
	}
	return &AuditLog{pool: pool}, nil
}

// Record appends an audit entry and returns its id. It NEVER silently drops a
// record: a write failure is returned to the caller (a mutation whose audit
// cannot be persisted should be treated as failed by the caller).
func (a *AuditLog) Record(ctx context.Context, e AuditEntry) (string, error) {
	if a == nil || a.pool == nil {
		return "", errors.New("platform: audit log not configured")
	}
	actor := strings.TrimSpace(e.ActorUserID)
	if actor == "" {
		return "", errors.New("platform: audit entry requires an actor")
	}
	action := strings.TrimSpace(e.Action)
	if action == "" {
		return "", errors.New("platform: audit entry requires an action")
	}

	var targetID *string
	if e.TargetTenantID != nil {
		s := e.TargetTenantID.String()
		targetID = &s
	}
	beforeJSON, err := marshalNullable(e.Before)
	if err != nil {
		return "", fmt.Errorf("platform: marshal before_state: %w", err)
	}
	afterJSON, err := marshalNullable(e.After)
	if err != nil {
		return "", fmt.Errorf("platform: marshal after_state: %w", err)
	}
	detailJSON, err := marshalNullable(e.Detail)
	if err != nil {
		return "", fmt.Errorf("platform: marshal detail: %w", err)
	}

	var id string
	err = a.pool.QueryRow(ctx, `
		INSERT INTO billing.platform_audit
			(actor_user_id, actor_org, action, target_tenant_id, reason, before_state, after_state, detail)
		VALUES ($1, NULLIF($2,''), $3, $4::uuid, NULLIF($5,''), $6::jsonb, $7::jsonb, $8::jsonb)
		RETURNING id::text
	`, actor, strings.TrimSpace(e.ActorOrg), action, targetID, strings.TrimSpace(e.Reason),
		beforeJSON, afterJSON, detailJSON).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("platform: record audit %q: %w", action, err)
	}

	fields := log.Fields{"audit_id": id, "actor": actor, "action": action}
	if targetID != nil {
		fields["target_tenant"] = *targetID
	}
	log.WithFields(fields).Info("platform: cross-tenant action audited")
	return id, nil
}

// List returns recent audit entries, most-recent first, optionally filtered to a
// target tenant. limit is clamped to a sane range.
func (a *AuditLog) List(ctx context.Context, targetTenant *tenant.ID, limit int) ([]AuditRow, error) {
	if a == nil || a.pool == nil {
		return nil, errors.New("platform: audit log not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	if targetTenant != nil {
		rows, err = a.pool.Query(ctx, `
			SELECT id::text, actor_user_id, COALESCE(actor_org,''), action,
			       COALESCE(target_tenant_id::text,''), COALESCE(reason,''), created_at
			  FROM billing.platform_audit
			 WHERE target_tenant_id = $1::uuid
			 ORDER BY created_at DESC LIMIT $2
		`, targetTenant.String(), limit)
	} else {
		rows, err = a.pool.Query(ctx, `
			SELECT id::text, actor_user_id, COALESCE(actor_org,''), action,
			       COALESCE(target_tenant_id::text,''), COALESCE(reason,''), created_at
			  FROM billing.platform_audit
			 ORDER BY created_at DESC LIMIT $1
		`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("platform: list audit: %w", err)
	}
	defer rows.Close()

	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.ActorUserID, &r.ActorOrg, &r.Action,
			&r.TargetTenantID, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func marshalNullable(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
