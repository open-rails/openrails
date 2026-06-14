package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrCustomerInvalid indicates an issuer/subject pair cannot identify an
// OpenRails payable subject.
var ErrCustomerInvalid = errors.New("controlplane: tenant subject issuer and subject are required")

// TouchCustomer resolves or creates the payable OpenRails customer for a
// merchant-scoped (issuer, subject) pair and updates last_seen_at. customers is
// UUID-only (#491): the customer id is the deterministic FederatedCustomerID of
// the triple, so the same pair always maps to the same balance without a lookup
// column.
func (c *ControlPlane) TouchCustomer(ctx context.Context, tenantID merchant.ID, issuer, subject string) (uuid.UUID, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if tenantID.IsZero() || issuer == "" || subject == "" {
		return uuid.Nil, ErrCustomerInvalid
	}
	if c == nil || c.pool == nil {
		return uuid.Nil, errors.New("controlplane: pgx pool unavailable for tenant subject resolution")
	}
	id := identity.FederatedCustomerID(tenantID.UUID(), issuer, subject).UUID()
	_, err := c.pool.Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id)
		VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
	`, id, tenantID.UUID())
	return id, err
}
