package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/tenant"
)

// MaxBreakGlassTTL caps the duration of a break-glass grant. Emergency elevation
// is time-boxed by design; a request for longer is clamped to this.
const MaxBreakGlassTTL = 4 * time.Hour

// DefaultBreakGlassTTL is used when a grant request omits a duration.
const DefaultBreakGlassTTL = 1 * time.Hour

// ErrBreakGlassJustificationRequired is returned when a grant has no written
// justification. Break-glass MUST be justified.
var ErrBreakGlassJustificationRequired = errors.New("platform: break-glass requires a written justification")

// ErrBreakGlassNotFound indicates no matching grant row.
var ErrBreakGlassNotFound = errors.New("platform: break-glass grant not found")

// BreakGlassGrant is a time-boxed elevation record.
type BreakGlassGrant struct {
	ID             string     `json:"id"`
	InvokerID      string     `json:"invoker_id"`
	TargetTenantID string     `json:"target_tenant_id,omitempty"`
	Justification  string     `json:"justification"`
	GrantedAt      time.Time  `json:"granted_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the grant is currently in force at now.
func (g BreakGlassGrant) Active(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	return now.Before(g.ExpiresAt)
}

// BreakGlass is the time-boxed break-glass elevation service (issue #226). Every
// grant requires a justification and an expiry, is persisted to
// billing.platform_break_glass, mirrored into the platform audit log, and emits
// a structured ALERT event (real paging is infra). It is gated upstream by the
// platform-superadmin permission.
type BreakGlass struct {
	pool  *pgxpool.Pool
	audit *AuditLog
	now   func() time.Time
}

// NewBreakGlass builds the break-glass service. pool and audit are required.
func NewBreakGlass(pool *pgxpool.Pool, audit *AuditLog) (*BreakGlass, error) {
	if pool == nil {
		return nil, errors.New("platform: break-glass requires a pgx pool")
	}
	if audit == nil {
		return nil, errors.New("platform: break-glass requires an audit log")
	}
	return &BreakGlass{pool: pool, audit: audit, now: time.Now}, nil
}

// GrantRequest parameterizes a break-glass elevation.
type GrantRequest struct {
	InvokerID     string
	ActorTenant   string
	TargetTenant  *tenant.ID // nil for a platform-wide elevation
	Justification string
	TTL           time.Duration // clamped to [0, MaxBreakGlassTTL]; 0 -> DefaultBreakGlassTTL
}

// Grant creates a time-boxed break-glass elevation. It:
//   - requires a non-empty justification,
//   - clamps the TTL to MaxBreakGlassTTL,
//   - persists the grant with an explicit expiry,
//   - audits it (platform_audit, action ActionBreakGlassGrant),
//   - emits a structured ALERT event for downstream paging.
func (b *BreakGlass) Grant(ctx context.Context, req GrantRequest) (*BreakGlassGrant, error) {
	if b == nil || b.pool == nil {
		return nil, errors.New("platform: break-glass not configured")
	}
	actor := strings.TrimSpace(req.InvokerID)
	if actor == "" {
		return nil, errors.New("platform: break-glass requires an actor")
	}
	just := strings.TrimSpace(req.Justification)
	if just == "" {
		return nil, ErrBreakGlassJustificationRequired
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultBreakGlassTTL
	}
	if ttl > MaxBreakGlassTTL {
		ttl = MaxBreakGlassTTL
	}

	now := b.now().UTC()
	expires := now.Add(ttl)

	var targetID *string
	if req.TargetTenant != nil {
		s := req.TargetTenant.String()
		targetID = &s
	}

	var g BreakGlassGrant
	var target string
	err := b.pool.QueryRow(ctx, `
		INSERT INTO billing.platform_break_glass
			(invoker_id, target_tenant_id, justification, granted_at, expires_at)
		VALUES ($1, $2::uuid, $3, $4, $5)
		RETURNING id::text, invoker_id, COALESCE(target_tenant_id::text,''),
		          justification, granted_at, expires_at, revoked_at
	`, actor, targetID, just, now, expires).Scan(
		&g.ID, &g.InvokerID, &target, &g.Justification, &g.GrantedAt, &g.ExpiresAt, &g.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("platform: insert break-glass grant: %w", err)
	}
	g.TargetTenantID = target

	// Audit the grant (cross-tenant record, survives tenant delete).
	if _, aerr := b.audit.Record(ctx, AuditEntry{
		InvokerID:      actor,
		ActorTenant:    req.ActorTenant,
		Action:         ActionBreakGlassGrant,
		TargetTenantID: req.TargetTenant,
		Reason:         just,
		After:          map[string]any{"grant_id": g.ID, "expires_at": g.ExpiresAt},
	}); aerr != nil {
		return nil, fmt.Errorf("platform: audit break-glass grant: %w", aerr)
	}

	// Emit a structured ALERT event. Real paging is infra (PagerDuty/Opsgenie);
	// this structured log line is the integration point a log-forwarder alerts on.
	log.WithFields(log.Fields{
		"alert":         "break_glass_granted",
		"severity":      "high",
		"grant_id":      g.ID,
		"actor":         actor,
		"target_tenant": target,
		"expires_at":    g.ExpiresAt.Format(time.RFC3339),
		"justification": just,
	}).Warn("ALERT: break-glass elevation granted")

	return &g, nil
}

// Revoke ends a grant early and audits the revocation. Idempotent: revoking an
// already-revoked or expired grant is not an error.
func (b *BreakGlass) Revoke(ctx context.Context, id, actorUserID, actorTenant string) error {
	if b == nil || b.pool == nil {
		return errors.New("platform: break-glass not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrBreakGlassNotFound
	}
	ct, err := b.pool.Exec(ctx, `
		UPDATE billing.platform_break_glass
		   SET revoked_at = current_timestamp
		 WHERE id = $1::uuid AND revoked_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("platform: revoke break-glass: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// Either it does not exist or it was already revoked; verify existence.
		var exists bool
		if e := b.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.platform_break_glass WHERE id=$1::uuid)`, id).Scan(&exists); e != nil {
			return fmt.Errorf("platform: check break-glass existence: %w", e)
		}
		if !exists {
			return ErrBreakGlassNotFound
		}
		return nil // already revoked: idempotent
	}
	if _, aerr := b.audit.Record(ctx, AuditEntry{
		InvokerID:   strings.TrimSpace(actorUserID),
		ActorTenant: actorTenant,
		Action:      ActionBreakGlassRevoke,
		Detail:      map[string]any{"grant_id": id},
	}); aerr != nil {
		return fmt.Errorf("platform: audit break-glass revoke: %w", aerr)
	}
	return nil
}

// IsActive reports whether an unrevoked, unexpired break-glass grant exists for
// the given actor (optionally scoped to a target tenant). Used to assert that an
// emergency elevation is in force before granting elevated cross-tenant access.
func (b *BreakGlass) IsActive(ctx context.Context, actorUserID string, targetTenant *tenant.ID) (bool, error) {
	if b == nil || b.pool == nil {
		return false, errors.New("platform: break-glass not configured")
	}
	actor := strings.TrimSpace(actorUserID)
	if actor == "" {
		return false, nil
	}
	now := b.now().UTC()
	var exists bool
	var err error
	if targetTenant != nil {
		err = b.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM billing.platform_break_glass
				 WHERE invoker_id = $1 AND revoked_at IS NULL AND expires_at > $2
				   AND (target_tenant_id IS NULL OR target_tenant_id = $3::uuid)
			)`, actor, now, targetTenant.String()).Scan(&exists)
	} else {
		err = b.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM billing.platform_break_glass
				 WHERE invoker_id = $1 AND revoked_at IS NULL AND expires_at > $2
			)`, actor, now).Scan(&exists)
	}
	if err != nil {
		return false, fmt.Errorf("platform: check active break-glass: %w", err)
	}
	return exists, nil
}

// ListActive returns all currently-active (unrevoked, unexpired) grants.
func (b *BreakGlass) ListActive(ctx context.Context) ([]BreakGlassGrant, error) {
	if b == nil || b.pool == nil {
		return nil, errors.New("platform: break-glass not configured")
	}
	now := b.now().UTC()
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, invoker_id, COALESCE(target_tenant_id::text,''),
		       justification, granted_at, expires_at, revoked_at
		  FROM billing.platform_break_glass
		 WHERE revoked_at IS NULL AND expires_at > $1
		 ORDER BY granted_at DESC
	`, now)
	if err != nil {
		return nil, fmt.Errorf("platform: list active break-glass: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

func scanGrants(rows pgx.Rows) ([]BreakGlassGrant, error) {
	var out []BreakGlassGrant
	for rows.Next() {
		var g BreakGlassGrant
		if err := rows.Scan(&g.ID, &g.InvokerID, &g.TargetTenantID,
			&g.Justification, &g.GrantedAt, &g.ExpiresAt, &g.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
