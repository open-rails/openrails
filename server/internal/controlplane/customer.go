package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/identity"
)

// ErrCustomerInvalid indicates a delegated request cannot identify an OpenRails
// payable subject.
var ErrCustomerInvalid = errors.New("controlplane: customer merchant and UUID subject are required")

// TouchCustomer resolves or creates the payable OpenRails customer for a
// delegated request and refreshes last_seen_at. Customer identity is the
// merchant plus the host/AuthKit stable UUID subject; issuer is audit metadata
// only and never participates in the natural key. The token's contact claims
// are recorded in the merchant's contacts, newest first; a failure to record
// them is logged and never refuses the request.
func (c *ControlPlane) TouchCustomer(ctx context.Context, merchantID billing.MerchantID, issuer, subject string, claims identity.Claims) (uuid.UUID, error) {
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
	if err := identity.RecordClaims(ctx, gen.New(c.pool), merchantID, row.ID, claims); err != nil {
		log.WithContext(ctx).WithError(err).WithField("customer_id", row.ID).Warn("controlplane: contact claims not recorded")
	}
	return row.ID, nil
}

// MerchantForSubject is one merchant a subject holds a customer record with:
// the directory identity a hosted portal needs to scope
// the subject's self-service billing to.
type MerchantForSubject struct {
	ID          billing.MerchantID
	Slug        string
	DisplayName string
}

// ListMerchantsForSubject returns the active merchants where subject has a
// customer record, ordered by slug. subject is the stable
// AuthKit UUID subject (the merchant-scoped customer ID). An empty subject
// yields no rows rather than an error.
//
// #824: this is a deliberately cross-merchant read — the hosted portal asks it
// BEFORE a merchant is chosen: a cross-merchant read of the subject's customer
// rows, joined to the global merchant directory.
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
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	out := make([]MerchantForSubject, 0, len(rows))
	for _, row := range rows {
		id := billing.MerchantID(row.ID)
		out = append(out, MerchantForSubject{ID: id, Slug: row.Slug, DisplayName: directory.DisplayName(ctx, id)})
	}
	return out, nil
}
