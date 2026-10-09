package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// accessCutover is the migration that makes product access the source of
// truth. It refuses any access change an operator has not approved.
const accessCutover = "0020_product_access_cutover.up.sql"

var errDryRun = errors.New("dry run")

// AccessCutoverPreflight applies the migrations before the cutover and
// dry-runs it for every merchant, in transactions it rolls back. With
// approvedBy, it records every listed change as approved, so the cutover
// applies exactly this report.
func AccessCutoverPreflight(ctx context.Context, pool *pgxpool.Pool, schema, approvedBy string) (billing.AccessCutoverReport, error) {
	report := billing.AccessCutoverReport{Changes: []billing.AccessChange{}, Notes: []billing.AccessNote{}}
	if pool == nil {
		return report, fmt.Errorf("missing postgres pool")
	}
	if schema == "" {
		schema = config.DefaultSchema
	}
	migrations, err := loadMigrations(schema)
	if err != nil {
		return report, err
	}
	cut := slices.IndexFunc(migrations, func(m migratekit.Migration) bool { return m.Name == accessCutover })
	if cut < 0 {
		return report, fmt.Errorf("openrails: migration %s is not embedded", accessCutover)
	}
	m, err := migratekit.NewPostgresFromPGXPool(pool, config.MigratekitApp)
	if err != nil {
		return report, fmt.Errorf("create OpenRails migrator: %w", err)
	}
	defer m.Close()
	m.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	status, err := m.Status(ctx, migrations)
	if err != nil {
		return report, err
	}
	if !slices.Contains(status.Pending, accessCutover) {
		return report, fmt.Errorf("openrails: the product access cutover already ran")
	}
	if err := m.ApplyMigrations(ctx, migrations[:cut]); err != nil {
		return report, fmt.Errorf("openrails: apply migrations before the cutover: %w", err)
	}
	database, err := db.NewWithPGXPool(pool, schema)
	if err != nil {
		return report, err
	}
	merchants, err := database.GenDirectory().ListAllMerchantIDs(ctx)
	if err != nil {
		return report, err
	}
	source := schema + ".entitlements"
	at := time.Now().UTC()
	for _, merchant := range merchants {
		err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := database.NewWithPgxTx(tx).Gen(ctx)
			if _, err := q.ConvertEntitlementWindows(ctx, gen.ConvertEntitlementWindowsParams{MerchantID: merchant, Source: source, At: at}); err != nil {
				return err
			}
			changes, err := q.ListProductAccessChanges(ctx, gen.ListProductAccessChangesParams{MerchantID: merchant, Source: source, At: at})
			if err != nil {
				return err
			}
			notes, err := q.ListProductAccessConversionNotes(ctx, gen.ListProductAccessConversionNotesParams{MerchantID: merchant, Source: source, At: at})
			if err != nil {
				return err
			}
			approvals, err := q.ListAccessCutoverApprovals(ctx, merchant)
			if err != nil {
				return err
			}
			approved := make(map[[3]string]bool, len(approvals))
			for _, a := range approvals {
				approved[[3]string{a.CustomerID.String(), a.Entitlement, a.Change}] = true
			}
			for _, c := range changes {
				report.Changes = append(report.Changes, billing.AccessChange{MerchantID: billing.MerchantID(merchant), CustomerID: billing.CustomerID(c.CustomerID), Entitlement: c.Entitlement, Change: billing.AccessChangeKind(c.Change),
					Approved: approved[[3]string{c.CustomerID.String(), c.Entitlement, c.Change}]})
			}
			for _, n := range notes {
				report.Notes = append(report.Notes, billing.AccessNote{MerchantID: billing.MerchantID(merchant), CustomerID: billing.CustomerID(n.CustomerID), Note: billing.AccessNoteKind(n.Note), SourceType: n.SourceType, SourceID: n.SourceID, Entitlements: n.Entitlements})
			}
			return errDryRun
		})
		if !errors.Is(err, errDryRun) {
			return report, fmt.Errorf("openrails: dry-run the cutover for merchant %s: %w", merchant, err)
		}
	}
	if approvedBy == "" {
		return report, nil
	}
	q := database.GenDirectory()
	for i, c := range report.Changes {
		if c.Approved {
			continue
		}
		if err := q.ApproveAccessCutoverChange(ctx, gen.ApproveAccessCutoverChangeParams{MerchantID: c.MerchantID.UUID(), CustomerID: c.CustomerID.UUID(), Entitlement: c.Entitlement, Change: string(c.Change), ApprovedBy: approvedBy}); err != nil {
			return report, err
		}
		report.Changes[i].Approved = true
	}
	return report, nil
}
