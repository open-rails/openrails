package hosttools

import (
	"context"

	"github.com/open-rails/openrails/internal/custodymigration"
)

// Custody migration: an operator hands over the manifest a custodian produced
// from a PSP vault export, and OpenRails flips each instrument's custody on the
// same payment_method_id, so the card is chargeable through another processor
// without touching any subscription. The implementation is
// internal/custodymigration; these aliases are the embedded vocabulary.
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

// MigrateCustody plans or applies one custodian vault-export manifest. Custody
// flips are merchant-scoped writes.
func MigrateCustody(ctx context.Context, opts CustodyMigrationOptions) (CustodyMigrationResult, error) {
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return CustodyMigrationResult{}, err
	}
	defer func() { _ = database.Close() }()
	return custodymigration.Migrate(ctx, opts)
}
