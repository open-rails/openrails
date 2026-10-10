package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// MerchantForSubject is one merchant a subject holds a customer record with:
// the directory identity a hosted portal needs to scope
// the subject's self-service billing to.
type MerchantForSubject struct {
	ID          billing.MerchantID
	Slug        string
	DisplayName string
}

// ListMerchantsForSubject returns the active merchants where subject, a user
// of the server's own AuthKit, is a customer, ordered by slug: a trusted
// issuer's customer with the same subject is someone else (OIDC Core §5.7).
// An empty subject yields no rows rather than an error.
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
		return nil, fmt.Errorf("%w: subject %q is not a UUID", billing.ErrInvalid, subject)
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
