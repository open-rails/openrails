package repo

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SystemCustomerID is the well-known payable subject that owns
// platform-initiated rows with no human principal (e.g. ledger repair alerts).
// It is a fixed, documented constant — deliberately NOT uuid.Nil, which stays
// the "no subject" zero value — and is materialized through the normal
// self-issuer path like any other UUID subject (#364).
var SystemCustomerID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// errNonUUIDSubject builds the rejection for non-UUID payable identities.
// OpenRails is UUID-only (#364): there is no legacy issuer, no generated row
// ids, no string subjects. The auth boundary rejects non-UUID subjects before
// they reach handlers; this repo-layer error is defense in depth.
func errNonUUIDSubject(userID string) error {
	return fmt.Errorf("merchant subject %q is not a UUID: payable identities are UUID-only (#364)", userID)
}

// EnsureCustomerID materializes (or refreshes) the openrails.customers
// row for a UUID subject and returns its id — which IS the subject UUID itself
// (#317). A non-UUID userID is rejected with an error (#364). An empty userID
// returns the zero id without touching the database (documented no-op for
// callers with optional identity).
//
// A zero tenantID falls back to the request's merchant context (required — an
// absent merchant is an error) — the same source the commerce tables use to stamp
// their own merchant_id column — so the resolved subject lands under the exact
// merchant the row itself belongs to. Multi-merchant writers that already hold an
// explicit merchant may pass it directly.
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
	return gen.New(qx).EnsureCustomer(ctx, gen.EnsureCustomerParams{
		ID:         uid,
		MerchantID: tenantID,
	})
}

// ResolveCustomerID derives the payable merchant subject id for a userID
// WITHOUT touching the database. Payable identities are UUID-only (#364), so a
// UUID subject IS its own id — pure derivation, no lookup. An empty userID
// yields the zero id (matches no rows, which callers translate to an empty
// result set); a non-UUID userID is an error.
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

// ensureCustomerRow makes sure a openrails.customers row exists for an
// already-resolved payable customer id, which the commerce repo Create methods
// call just before insert so the FK target exists (#317). customers is UUID-only
// (#491): the row is materialized as (id, merchant_id); the ON CONFLICT makes a
// repeat a no-op. A zero id is a no-op (the caller must set model.CustomerID
// before Create).
func ensureCustomerRow(ctx context.Context, qx gen.DBTX, tenantID uuid.UUID, tsid uuid.UUID) error {
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
	return gen.New(qx).EnsureCustomerRow(ctx, gen.EnsureCustomerRowParams{
		ID:         tsid,
		MerchantID: tenantID,
	})
}
