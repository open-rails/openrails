package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/grants"
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
	Config       *config.Config
	PGXPool      *pgxpool.Pool
	MerchantSlug string
	Grants       []AdminGrant
}

// AdminGrantImportResult reports per-source outcomes of an import batch so the
// host preserves its row-level audit trail. The lists hold AdminGrant.SourceIDs.
type AdminGrantImportResult struct {
	Imported []string // newly recorded as admin grants (+ entitlement materialized)
	Skipped  []string // SourceID already imported (idempotent re-run)
	NoSpec   []string // product has no entitlements_spec → nothing to grant
	Blocked  []string // grant recorded, but every feature's window overlapped a live window → no window materialized (#695)
}

// ImportAdminGrants records each admin comp as a source_type=admin entitlement
// grant and materializes its entitlement window, idempotent by SourceID (#636).
// This is the seam that lets the doujins legacy migrate hand admin/manual access
// over as a FACT instead of writing openrails.entitlements directly. Runs in a
// single merchant-scoped connection (RLS). A real DB error aborts the batch; a
// product with no entitlements_spec is counted (NoSpec) and skipped.
func ImportAdminGrants(ctx context.Context, opts AdminGrantImportOptions) (AdminGrantImportResult, error) {
	var res AdminGrantImportResult
	if ctx == nil {
		ctx = context.Background()
	}
	if len(opts.Grants) == 0 {
		return res, nil
	}
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return res, err
	}
	defer database.Close()

	merchantID, err := db.ResolveMerchantSlug(ctx, database.Pool(), strings.TrimSpace(opts.MerchantSlug))
	if err != nil {
		return res, fmt.Errorf("import admin grants: resolve merchant %q: %w", opts.MerchantSlug, err)
	}
	ctx = merchant.WithID(ctx, merchantID)

	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		gl := grants.New(database.Gen(ctx), merchantID.UUID())
		specCache := map[uuid.UUID][]string{}
		for _, g := range opts.Grants {
			feats, ok := specCache[g.Product]
			if !ok {
				prod, err := database.Gen(ctx).GetProductByID(ctx, g.Product)
				if err != nil {
					return fmt.Errorf("import admin grants: load product %s: %w", g.Product, err)
				}
				feats = productEntitlementKeys(prod.EntitlementsSpec)
				specCache[g.Product] = feats
			}
			if len(feats) == 0 {
				res.NoSpec = append(res.NoSpec, g.SourceID)
				continue
			}
			created, alreadyExists, err := gl.GrantAdmin(ctx, g.Customer, g.SourceID, feats, g.StartsAt, g.EndsAt)
			if err != nil {
				return fmt.Errorf("import admin grant %s: %w", g.SourceID, err)
			}
			switch {
			case alreadyExists:
				res.Skipped = append(res.Skipped, g.SourceID)
			case created > 0:
				res.Imported = append(res.Imported, g.SourceID)
			default:
				res.Blocked = append(res.Blocked, g.SourceID) // all features overlapped
			}
		}
		return nil
	})
	return res, err
}

// productEntitlementKeys returns the entitlement feature names — the keys of a
// product's entitlements_spec ({name: hours} JSONB).
func productEntitlementKeys(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
