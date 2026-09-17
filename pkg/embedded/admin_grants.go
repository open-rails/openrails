package embedded

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdminGrant is an operator/manual "comp": a customer was granted a product's
// entitlements for a window with NO payment or subscription behind it. It is a
// source-of-truth FACT the host hands over (the openrails grant ledger is the
// provenance), NOT a derived entitlement effect — convergence derives the
// entitlement. SourceID is the host's stable id for the comp (idempotency key).
// EndsAt nil = indefinite.
type AdminGrant struct {
	Customer uuid.UUID
	Product  uuid.UUID
	SourceID string
	StartsAt time.Time
	EndsAt   *time.Time
}

// AdminGrantImportOptions configures ImportAdminGrants.
type AdminGrantImportOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID merchant.ID
	Grants     []AdminGrant
}

// AdminGrantImportResult reports per-source outcomes of an import batch so the
// host preserves its row-level audit trail. The lists hold AdminGrant.SourceIDs.
type AdminGrantImportResult struct {
	Imported []string // newly recorded as admin grants (+ entitlement materialized)
	Skipped  []string // SourceID already imported (idempotent re-run)
	NoSpec   []string // product has no entitlements_spec → nothing to grant
	Blocked  []string // grant recorded, but every feature's window overlapped a live window → no window materialized (#695)
}

// ImportAdminGrants records admin comps through the declared-facts import
// (DeclaredBilling.AdminGrants) so hosts keep one door for legacy facts.
func ImportAdminGrants(ctx context.Context, opts AdminGrantImportOptions) (AdminGrantImportResult, error) {
	var res AdminGrantImportResult
	if len(opts.Grants) == 0 {
		return res, nil
	}
	declared := make([]billingimport.DeclaredAdminGrant, 0, len(opts.Grants))
	for _, g := range opts.Grants {
		declared = append(declared, billingimport.DeclaredAdminGrant{Customer: g.Customer, Product: g.Product, SourceID: g.SourceID, StartsAt: g.StartsAt, EndsAt: g.EndsAt})
	}
	out, err := ImportBilling(ctx, BillingImportOptions{Config: opts.Config, PGXPool: opts.PGXPool, MerchantID: opts.MerchantID,
		Book: DeclaredBilling{AsOf: time.Now().UTC(), AdminGrants: declared}})
	if err != nil {
		return res, err
	}
	res.Imported, res.Skipped = out.Imported, out.Skipped
	for _, src := range out.Blocked {
		if out.Reasons[src] == "product has no entitlements_spec" {
			res.NoSpec = append(res.NoSpec, src)
			continue
		}
		res.Blocked = append(res.Blocked, src)
	}
	return res, nil
}
