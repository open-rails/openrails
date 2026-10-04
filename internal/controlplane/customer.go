package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// ErrCustomerInvalid indicates a delegated request cannot identify an OpenRails
// payable subject.
var ErrCustomerInvalid = errors.New("controlplane: customer merchant and UUID subject are required")

// TouchCustomer resolves or creates the payable OpenRails customer for a
// delegated request and refreshes last_seen_at. Customer identity is the
// merchant plus the host/AuthKit stable UUID subject; issuer is audit metadata
// only and never participates in the natural key.
func (c *ControlPlane) TouchCustomer(ctx context.Context, merchantID billing.MerchantID, issuer, subject string) (uuid.UUID, error) {
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
	q := gen.New(tx)
	if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: db.MerchantGUC, Value: merchantID.String(), IsLocal: true}); err != nil {
		return uuid.Nil, err
	}

	var issuerPtr *string
	if issuer != "" {
		issuerPtr = &issuer
	}
	row, err := q.EnsureCustomer(ctx, gen.EnsureCustomerParams{
		MerchantID: merchantID.UUID(),
		Issuer:     issuerPtr,
		ID:         subjectID,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// MerchantForSubject is one merchant a subject holds a customer record with
// (openrails-saas #18): the directory identity a hosted portal needs to scope
// the subject's self-service billing to.
type MerchantForSubject struct {
	ID          billing.MerchantID
	Slug        string
	DisplayName string
}

// ListMerchantsForSubject returns the active merchants where subject has a
// customer record, ordered by slug (openrails-saas #18). subject is the stable
// AuthKit UUID subject (the merchant-scoped customer ID). An empty subject
// yields no rows rather than an error.
//
// #824: this is a deliberately cross-merchant read — the hosted portal asks it
// BEFORE a merchant is chosen. As a plain pool query under the since-removed
// RLS, the customers half of the join matched nothing and the portal's
// merchant list was always EMPTY. The customers lookup now goes through the
// SECURITY DEFINER directory function (migration 0016); billing.merchants is
// a global table, so the rest is an ordinary query.
func (c *ControlPlane) ListMerchantsForSubject(ctx context.Context, subject string) ([]MerchantForSubject, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, nil
	}
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for merchant enumeration")
	}
	subjectID, err := uuid.Parse(subject)
	if err != nil {
		return nil, ErrCustomerInvalid
	}
	rows, err := gen.New(c.pool).ListMerchantsForCustomerSubject(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantForSubject, 0, len(rows))
	for _, row := range rows {
		out = append(out, MerchantForSubject{ID: billing.MerchantID(row.ID), Slug: row.Slug, DisplayName: row.DisplayName})
	}
	return out, nil
}
