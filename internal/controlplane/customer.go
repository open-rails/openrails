package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrCustomerInvalid indicates an issuer/subject pair cannot identify an
// OpenRails payable subject.
var ErrCustomerInvalid = errors.New("controlplane: tenant subject issuer and subject are required")

// TouchCustomer resolves or creates the payable OpenRails customer for a
// delegated (issuer, subject) request and refreshes last_seen_at. #491: the
// payer is resolved by the issuer's ORG-BINDING (authkit#80 nullable org_id):
//   - ORG-BOUND issuer  -> ONE payer per org    : UNIQUE(merchant_id, org_id)
//   - ORG-LESS issuer   -> per (issuer, subject): UNIQUE(merchant_id, issuer, subject)
//
// Both are resolved via INSERT ... ON CONFLICT (<natural key>) RETURNING id, so
// the id is a uuidv7 minted once and looked up by the natural key (the
// deterministic uuidv5 FederatedCustomerID is gone). The SUBJECT is the invoker
// in both branches, never a customer in the org-bound branch.
func (c *ControlPlane) TouchCustomer(ctx context.Context, tenantID merchant.ID, issuer, subject string) (uuid.UUID, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if tenantID.IsZero() || issuer == "" || subject == "" {
		return uuid.Nil, ErrCustomerInvalid
	}
	if c == nil || c.pool == nil {
		return uuid.Nil, errors.New("controlplane: pgx pool unavailable for tenant subject resolution")
	}

	// Org-binding switch (authkit#80): empty org => org-less branch.
	orgID := ""
	if core := c.Core(); core != nil {
		var oerr error
		orgID, oerr = core.ResolveRemoteApplicationOrg(ctx, issuer)
		if oerr != nil {
			return uuid.Nil, oerr
		}
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // harmless after Commit
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, TRUE)", db.MerchantGUC, tenantID.String()); err != nil {
		return uuid.Nil, err
	}

	q := gen.New(tx)
	var id uuid.UUID
	if strings.TrimSpace(orgID) != "" {
		id, err = q.UpsertCustomerByOrg(ctx, gen.UpsertCustomerByOrgParams{
			MerchantID: tenantID.UUID(),
			OrgID:      &orgID,
		})
	} else {
		id, err = q.UpsertCustomerByIssuerSubject(ctx, gen.UpsertCustomerByIssuerSubjectParams{
			MerchantID: tenantID.UUID(),
			Issuer:     &issuer,
			Subject:    &subject,
		})
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
