package embedded

import (
	"context"

	"github.com/open-rails/openrails/internal/custodymigration"
)

// or#297 Phase C: the vault-import + token-remap seam. An operator hands over
// the manifest the custodian produced after ingesting a PSP vault export, and
// OpenRails flips each instrument's CUSTODY on the same payment_method_id —
// the move that makes a de-platformed card book chargeable again through a
// different processor, without touching a single subscription.
//
// Implementation and full doctrine live in internal/custodymigration (shared
// with the HTTP surface without an import cycle); these aliases are the stable
// embedded vocabulary.
type (
	VaultExport   = custodymigration.VaultExport
	ImportedToken = custodymigration.ImportedToken
	CustodyPSPRef = custodymigration.PSPRef

	// CustodyMigrationOptions configures MigrateCustody. Apply=false is a DRY
	// RUN — plan first.
	CustodyMigrationOptions = custodymigration.Options
	// CustodyMigrationResult reports counts by outcome plus a per-token verdict.
	CustodyMigrationResult = custodymigration.Result
	// CustodyMigrationRow is one manifest line's verdict.
	CustodyMigrationRow = custodymigration.RowResult
	// CustodyOutcome is the per-token verdict vocabulary.
	CustodyOutcome = custodymigration.Outcome
)

// Outcome vocabulary, re-exported so hosts can branch on it without importing
// internal packages.
const (
	CustodyRemapped        = custodymigration.OutcomeRemapped
	CustodyCreated         = custodymigration.OutcomeCreated
	CustodyAlreadyMigrated = custodymigration.OutcomeAlreadyMigrated
	CustodyUnmatched       = custodymigration.OutcomeUnmatched
	CustodyBlocked         = custodymigration.OutcomeBlocked
)

// MigrateCustody plans or applies one custodian vault-export manifest. The
// host's pool passes the same RLS-posture gate `embedded.New` applies
// (or#885): custody flips are merchant-scoped writes, and under a privileged
// role the merchant_isolation policies that make that scoping real are skipped.
func MigrateCustody(ctx context.Context, opts CustodyMigrationOptions) (CustodyMigrationResult, error) {
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return CustodyMigrationResult{}, err
	}
	defer func() { _ = database.Close() }()
	return custodymigration.Migrate(ctx, opts)
}
