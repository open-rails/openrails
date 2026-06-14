package repo

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SelfIssuer keys openrails.tenant_subjects rows for self-service identities whose
// external subject IS a UUID (the same value the credits/self-service hot path
// uses directly as the payable tenant_subject_id). For these the row id equals the
// subject UUID, so commerce and credit rows reference one payable subject (#317).
const SelfIssuer = "openrails:self"

// SystemMerchantSubjectID is the well-known payable subject that owns
// platform-initiated rows with no human principal (e.g. ledger repair alerts).
// It is a fixed, documented constant — deliberately NOT uuid.Nil, which stays
// the "no subject" zero value — and is materialized through the normal
// self-issuer path like any other UUID subject (#364).
var SystemMerchantSubjectID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// errNonUUIDSubject builds the rejection for non-UUID payable identities.
// OpenRails is UUID-only (#364): there is no legacy issuer, no generated row
// ids, no string subjects. The auth boundary rejects non-UUID subjects before
// they reach handlers; this repo-layer error is defense in depth.
func errNonUUIDSubject(userID string) error {
	return fmt.Errorf("tenant subject %q is not a UUID: payable identities are UUID-only (#364)", userID)
}

// EnsureMerchantSubjectID materializes (or refreshes) the openrails.tenant_subjects
// row for a UUID subject and returns its id — which IS the subject UUID itself
// (#317). A non-UUID userID is rejected with an error (#364). An empty userID
// returns the zero id without touching the database (documented no-op for
// callers with optional identity).
//
// A zero tenantID falls back to the request's tenant context (required — an
// absent tenant is an error) — the same source the commerce tables use to stamp
// their own tenant_id column — so the resolved subject lands under the exact
// tenant the row itself belongs to. Multi-tenant writers that already hold an
// explicit tenant may pass it directly.
func EnsureMerchantSubjectID(ctx context.Context, qx gen.DBTX, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
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
	return gen.New(qx).UpsertSelfMerchantSubject(ctx, gen.UpsertSelfMerchantSubjectParams{
		ID:       uid,
		MerchantID: tenantID,
		Issuer:   SelfIssuer,
		Subject:  uid.String(),
	})
}

// ResolveMerchantSubjectID derives the payable tenant subject id for a userID
// WITHOUT touching the database. Payable identities are UUID-only (#364), so a
// UUID subject IS its own id — pure derivation, no lookup. An empty userID
// yields the zero id (matches no rows, which callers translate to an empty
// result set); a non-UUID userID is an error.
func ResolveMerchantSubjectID(userID string) (uuid.UUID, error) {
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

// ensureMerchantSubjectRow makes sure a openrails.tenant_subjects row exists for an
// already-resolved payable tenant subject id, which the commerce repo Create
// methods call just before insert so the FK target exists (#317). For a
// self-service subject the id IS the subject UUID, so the row is materialized as
// (id, tenant, 'openrails:self', id::text); a federated subject already has its
// row and the ON CONFLICT makes this a no-op. A zero id is a no-op (the caller
// must set model.MerchantSubjectID before Create).
func ensureMerchantSubjectRow(ctx context.Context, qx gen.DBTX, tenantID uuid.UUID, tsid uuid.UUID) error {
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
	return gen.New(qx).EnsureMerchantSubjectRow(ctx, gen.EnsureMerchantSubjectRowParams{
		ID:       tsid,
		MerchantID: tenantID,
		Issuer:   SelfIssuer,
		Subject:  tsid.String(),
	})
}
