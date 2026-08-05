package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// systemCustomerNamespace permanently anchors the per-merchant payable subject
// that owns platform-initiated rows with no human principal (for example,
// ledger repair alerts). It must not change after per-merchant IDs ship because
// persisted notification rows refer to the IDs derived from it.
var systemCustomerNamespace = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// SystemCustomerID derives the well-known system payable subject for merchantID.
// The merchant participates in the identity because customers.id is globally
// unique while customer rows are isolated by merchant RLS (#889).
func SystemCustomerID(merchantID uuid.UUID) uuid.UUID {
	return uuidutil.DeterministicID(systemCustomerNamespace, merchantID.String())
}

// ErrCustomerOwnedByAnotherMerchant signals that a customer id already belongs
// to a DIFFERENT merchant. customers.id is globally unique while customer rows
// are merchant-isolated (#889), so a foreign id must be refused loudly: silently
// re-pointing it (privileged upsert) or silently no-op'ing (ON CONFLICT DO
// NOTHING, which then lets the caller's row FK into another merchant's customer,
// because FK checks bypass RLS) is cross-merchant corruption that logs success.
var ErrCustomerOwnedByAnotherMerchant = errors.New("customer id is already owned by another merchant")

// errNonUUIDSubject builds the rejection for non-UUID payable identities.
// OpenRails is UUID-only (#364): there is no legacy issuer, no generated row
// ids, no string subjects. The auth boundary rejects non-UUID subjects before
// they reach handlers; this error is defense in depth.
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
	id, err := gen.New(qx).EnsureCustomer(ctx, gen.EnsureCustomerParams{
		ID:         uid,
		MerchantID: tenantID,
		Subject:    &userID,
	})
	if err != nil {
		return uuid.Nil, customerOwnershipError(err, uid, tenantID)
	}
	return id, nil
}

// customerOwnershipError names the cross-merchant claim behind an empty upsert
// result: the guarded ON CONFLICT matches no row exactly when the id belongs to
// another merchant (#889).
func customerOwnershipError(err error, id, merchantID uuid.UUID) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: customer %s under merchant %s", ErrCustomerOwnedByAnotherMerchant, id, merchantID)
	}
	return err
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

// EnsureCustomerRow makes sure a openrails.customers row exists for an
// already-resolved payable customer id, which the commerce Create methods
// call just before insert so the FK target exists (#317). customers is UUID-only
// (#491): the row is materialized as (id, merchant_id); the ON CONFLICT makes a
// repeat a no-op. A zero id is a no-op (the caller must set model.CustomerID
// before Create).
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
	subject := tsid.String()
	_, err := q.EnsureCustomerRow(ctx, gen.EnsureCustomerRowParams{
		ID:         tsid,
		MerchantID: tenantID,
		Subject:    &subject,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// No row back means either a foreign owner or a race: ON CONFLICT DO NOTHING
	// returns nothing for a row this statement's snapshot cannot see, including
	// one a concurrent first-touch committed after the snapshot was taken. A
	// locking re-read settles it — it waits out the other writer and sees the
	// committed row (#889).
	if _, lerr := q.LockCustomerForMerchant(ctx, gen.LockCustomerForMerchantParams{
		ID: tsid, MerchantID: tenantID,
	}); lerr != nil {
		return customerOwnershipError(lerr, tsid, tenantID)
	}
	return nil
}
