package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// errNonUUIDSubject rejects a non-UUID payable identity. The auth boundary
// already refuses them; this is defense in depth.
func errNonUUIDSubject(userID string) error {
	return fmt.Errorf("merchant subject %q is not a UUID: payable identities are UUID-only (#364)", userID)
}

// EnsureCustomerID upserts the customers row for a UUID subject and returns
// its id, which is the subject UUID. An empty userID returns the zero id
// without touching the database; a non-UUID one is an error. A zero tenantID
// means the context's merchant, which is required.
func EnsureCustomerID(ctx context.Context, qx gen.DBTX, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	uid, perr := uuid.Parse(userID)
	if perr != nil {
		return uuid.Nil, errNonUUIDSubject(userID)
	}
	if tenantID == uuid.Nil {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return uuid.Nil, terr
		}
		tenantID = tid.UUID()
	}
	row, err := gen.New(qx).EnsureCustomer(ctx, gen.EnsureCustomerParams{
		ID:         uid,
		MerchantID: tenantID,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// ResolveCustomerID derives a customer id without touching the database: a
// UUID subject is its own id. An empty userID yields the zero id (matches no
// rows); a non-UUID userID is an error.
func ResolveCustomerID(userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return uuid.Nil, errNonUUIDSubject(userID)
	}
	return uid, nil
}

// EnsureCustomerRow makes sure the customers row (id, merchant_id) exists
// before a commerce insert references it; a repeat is a no-op. A zero id is a
// no-op, so the caller must set model.CustomerID first.
func EnsureCustomerRow(ctx context.Context, qx gen.DBTX, tenantID uuid.UUID, tsid uuid.UUID) error {
	return EnsureCustomerRowQ(ctx, gen.New(qx), tenantID, tsid)
}

// EnsureCustomerRowQ is EnsureCustomerRow for callers already holding a
// *gen.Queries bound to their transaction.
func EnsureCustomerRowQ(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, tsid uuid.UUID) error {
	if tsid == uuid.Nil {
		return nil
	}
	if tenantID == uuid.Nil {
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID = tid.UUID()
	}
	return q.EnsureCustomerRow(ctx, gen.EnsureCustomerRowParams{
		ID:         tsid,
		MerchantID: tenantID,
	})
}

// ErrCustomerIssuerMismatch: the customer is another issuer's subject.
var ErrCustomerIssuerMismatch = errors.New("db: the customer belongs to another issuer")

// AdmitCustomer admits issuer's subject id as merchantID's customer: an
// existing customer only when its issuer is issuer, else a new one made
// with it. "" is the host's own users (the native issuer); a trusted
// issuer's subject never reaches a customer of another issuer, the host's
// own included (OIDC Core §5.7).
func AdmitCustomer(ctx context.Context, q *gen.Queries, merchantID, id uuid.UUID, issuer string) error {
	want := strings.TrimSpace(issuer)
	for attempt := 0; ; attempt++ {
		row, err := q.GetCustomer(ctx, gen.GetCustomerParams{MerchantID: merchantID, ID: id})
		switch {
		case err == nil:
			have := ""
			if row.Issuer != nil {
				have = *row.Issuer
			}
			if have != want {
				return ErrCustomerIssuerMismatch
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows) || attempt > 0:
			return err
		}
		var stored *string
		if want != "" {
			stored = &want
		}
		if err := q.CreateCustomer(ctx, gen.CreateCustomerParams{ID: id, MerchantID: merchantID, Issuer: stored}); err != nil {
			return err
		}
	}
}
