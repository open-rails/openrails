package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	if errors.Is(err, pgx.ErrNoRows) {
		// The guarded upsert matches no row when the subject is already a
		// customer of a DIFFERENT merchant (#889). One AuthKit instance can
		// serve several merchants, so refuse instead of handing back an id the
		// merchant does not own.
		return uuid.Nil, fmt.Errorf("%w: subject %s under merchant %s", db.ErrCustomerOwnedByAnotherMerchant, subjectID, merchantID)
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// MerchantForSubject is one merchant a subject holds a customer record with
// (openrails-saas #18): the directory identity a hosted portal needs to scope
// the subject's self-service billing to.
type MerchantForSubject struct {
	Slug        string
	DisplayName string
}

// ListMerchantsForSubject returns the active merchants where subject has a
// customer record, ordered by slug (openrails-saas #18). It runs on the
// control-plane pool — the privileged, non-RLS role — so it reads across every
// merchant scope in one query; the per-merchant openrails_app role could never
// see another merchant's customers. subject is the stable AuthKit UUID subject
// (matched against customers.subject, as TouchCustomer stores it). An empty
// subject yields no rows rather than an error.
func (c *ControlPlane) ListMerchantsForSubject(ctx context.Context, subject string) ([]MerchantForSubject, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, nil
	}
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for merchant enumeration")
	}
	rows, err := c.pool.Query(ctx, `
		SELECT m.slug, COALESCE(m.display_name, '')
		  FROM openrails.merchants m
		 WHERE m.deleted_at IS NULL
		   AND m.status = 'active'
		   AND EXISTS (
		       SELECT 1
		         FROM openrails.customers c
		        WHERE c.merchant_id = m.id
		          AND c.subject = $1
		   )
		 ORDER BY m.slug
	`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MerchantForSubject
	for rows.Next() {
		var m MerchantForSubject
		if err := rows.Scan(&m.Slug, &m.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
