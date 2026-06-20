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

// ErrCustomerInvalid indicates a delegated request cannot identify an OpenRails
// payable subject.
var ErrCustomerInvalid = errors.New("controlplane: customer merchant and UUID subject are required")

// TouchCustomer resolves or creates the payable OpenRails customer for a
// delegated request and refreshes last_seen_at. Customer identity is the
// merchant plus the host/AuthKit stable UUID subject; issuer is audit metadata
// only and never participates in the natural key.
func (c *ControlPlane) TouchCustomer(ctx context.Context, merchantID merchant.ID, issuer, subject string) (uuid.UUID, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if merchantID.IsZero() || subject == "" {
		return uuid.Nil, ErrCustomerInvalid
	}
	subjectID, err := uuid.Parse(subject)
	if err != nil {
		return uuid.Nil, ErrCustomerInvalid
	}
	if c == nil || c.pool == nil {
		return uuid.Nil, errors.New("controlplane: pgx pool unavailable for customer resolution")
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // harmless after Commit
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, TRUE)", db.MerchantGUC, merchantID.String()); err != nil {
		return uuid.Nil, err
	}

	q := gen.New(tx)
	var issuerPtr *string
	if issuer != "" {
		issuerPtr = &issuer
	}
	id, err := q.UpsertCustomerBySubject(ctx, gen.UpsertCustomerBySubjectParams{
		MerchantID: merchantID.UUID(),
		Issuer:     issuerPtr,
		Subject:    &subjectID,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
