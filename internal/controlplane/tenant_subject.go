package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/tenant"
)

// ErrTenantSubjectInvalid indicates an issuer/subject pair cannot identify an
// OpenRails payable subject.
var ErrTenantSubjectInvalid = errors.New("controlplane: tenant subject issuer and subject are required")

// TouchTenantSubject resolves or creates the payable OpenRails subject for a
// tenant-scoped OIDC issuer+subject pair and updates last_seen_at.
func (c *ControlPlane) TouchTenantSubject(ctx context.Context, tenantID tenant.ID, issuer, subject string) (uuid.UUID, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if tenantID.IsZero() || issuer == "" || subject == "" {
		return uuid.Nil, ErrTenantSubjectInvalid
	}
	if c == nil || c.pool == nil {
		return uuid.Nil, errors.New("controlplane: pgx pool unavailable for tenant subject resolution")
	}
	var id uuid.UUID
	err := c.pool.QueryRow(ctx, `
		INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, issuer, subject) DO UPDATE
		   SET last_seen_at = now()
		RETURNING id
	`, tenantID.UUID(), issuer, subject).Scan(&id)
	return id, err
}
