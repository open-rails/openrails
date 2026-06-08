package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/pkg/tenant"
)

// LegacyUserIssuer is the synthetic OIDC issuer used to key billing.tenant_subjects
// rows for pre-hard-cut commerce identities whose external identity is just a
// user_id string (#317). It matches the issuer the 074/075 backfills used, so
// resolution reuses the backfilled rows rather than creating duplicates.
const LegacyUserIssuer = "openrails:legacy-user"

// SelfIssuer keys billing.tenant_subjects rows for self-service identities whose
// external subject IS a UUID (the same value the credits/self-service hot path
// uses directly as the payable tenant_subject_id). For these the row id equals the
// subject UUID, so commerce and credit rows reference one payable subject (#317).
const SelfIssuer = "openrails:self"

// EnsureTenantSubjectID resolves (or creates) the billing.tenant_subjects row for
// (tenantID, LegacyUserIssuer, userID) and returns its id, refreshing
// last_seen_at. It is the repo-layer resolver commerce writers use to stamp the
// payable tenant_subject_id on rows that still carry a legacy user_id while the
// hard cut (#317) is rolled out table by table.
//
// A zero tenantID falls back to the request's tenant context (and, absent that,
// the default tenant) — the same source the commerce tables use to stamp their
// own tenant_id column — so the resolved subject lands under the exact tenant the
// row itself belongs to. Multi-tenant writers that already hold an explicit
// tenant may pass it directly. An empty userID returns the zero id without
// touching the database (nothing to resolve).
func EnsureTenantSubjectID(ctx context.Context, db bun.IDB, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}

	// Self-service identities are their OWN UUID: the credits/self-service hot path
	// uses identity.TenantSubjectIDFromString(user.ID) — i.e. the user UUID itself —
	// as the payable tenant_subject_id, with no tenant_subjects row. To converge
	// commerce and credit rows on ONE payable subject id, when the user_id is a
	// UUID we materialize the tenant_subjects row WITH id = that UUID and return it
	// unchanged. If a federated/self-service row with that payable id already
	// exists, refresh last_seen_at and return it; the id is the identity here.
	if uid, perr := uuid.Parse(userID); perr == nil {
		var id uuid.UUID
		err := db.NewRaw(
			`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
			 RETURNING id`,
			uid, tenantID, SelfIssuer, userID,
		).Scan(ctx, &id)
		return id, err
	}

	// Non-UUID external subject: key by the legacy-user issuer with a generated id.
	var id uuid.UUID
	err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
		 VALUES (?, ?, ?)
		 ON CONFLICT (tenant_id, issuer, subject) DO UPDATE SET last_seen_at = now()
		 RETURNING id`,
		tenantID, LegacyUserIssuer, userID,
	).Scan(ctx, &id)
	return id, err
}

// ResolveTenantSubjectID returns the payable tenant subject id for a legacy
// (tenantID, userID) identity WITHOUT creating a row — the read-side counterpart
// of EnsureTenantSubjectID, used to convert `WHERE user_id = ?` read queries to
// `WHERE tenant_subject_id = ?` (#317).
//
// Self-service identities are their own UUID, so a UUID userID resolves to itself
// with no database round-trip (matching the deterministic scheme the write path
// and migrations 075–077 use). A non-UUID external subject is looked up under the
// legacy-user issuer; an unknown subject yields the zero id (no matching rows),
// which callers translate to an empty result set.
func ResolveTenantSubjectID(ctx context.Context, db bun.IDB, tenantID uuid.UUID, userID string) (uuid.UUID, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return uuid.Nil, nil
	}
	if uid, perr := uuid.Parse(userID); perr == nil {
		return uid, nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	var id uuid.UUID
	err := db.NewRaw(
		`SELECT id FROM billing.tenant_subjects
		  WHERE tenant_id = ? AND issuer = ? AND subject = ?`,
		tenantID, LegacyUserIssuer, userID,
	).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return id, err
}

// ensureTenantSubjectRow makes sure a billing.tenant_subjects row exists for an
// already-resolved payable tenant subject id, which the commerce repo Create
// methods call just before insert so the FK target exists (#317). For a
// self-service subject the id IS the subject UUID, so the row is materialized as
// (id, tenant, 'openrails:self', id::text); a federated subject already has its
// row and the ON CONFLICT makes this a no-op. A zero id is a no-op (the caller
// must set model.TenantSubjectID before Create).
func ensureTenantSubjectRow(ctx context.Context, db bun.IDB, tenantID uuid.UUID, tsid uuid.UUID) error {
	if tsid == uuid.Nil {
		return nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	_, err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`,
		tsid, tenantID, SelfIssuer, tsid.String(),
	).Exec(ctx)
	return err
}
