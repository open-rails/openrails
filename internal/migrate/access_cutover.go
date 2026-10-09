package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// accessCutover is the migration that makes product access the source of
// truth. It refuses any access change an operator has not approved.
const accessCutover = "0020_product_access_cutover.up.sql"

// AccessChange is one key whose access the cutover changes for a customer
// from now on: lost (their product dropped it after they got it) or gained.
type AccessChange struct {
	MerchantID  uuid.UUID `json:"merchant_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Entitlement string    `json:"entitlement"`
	Change      string    `json:"change"`
	Approved    bool      `json:"approved"`
}

// AccessNote explains a conversion besides key changes: live windows it could
// not carry (unmapped) and purchases whose keys had different durations
// (mixed_duration: they keep the longest).
type AccessNote struct {
	MerchantID   uuid.UUID `json:"merchant_id"`
	CustomerID   uuid.UUID `json:"customer_id"`
	Note         string    `json:"note"`
	SourceType   string    `json:"source_type"`
	SourceID     string    `json:"source_id"`
	Entitlements []string  `json:"entitlements"`
}

// AccessCutoverReport is what the cutover would change, per customer and key.
type AccessCutoverReport struct {
	Changes []AccessChange `json:"changes"`
	Notes   []AccessNote   `json:"notes"`
}

// Unapproved counts the changes the cutover would refuse.
func (r AccessCutoverReport) Unapproved() int {
	n := 0
	for _, c := range r.Changes {
		if !c.Approved {
			n++
		}
	}
	return n
}

var errDryRun = errors.New("dry run")

// AccessCutoverPreflight applies the migrations before the cutover and
// dry-runs it for every merchant, in transactions it rolls back. With
// approvedBy, it records every listed change as approved, so the cutover
// applies exactly this report.
func AccessCutoverPreflight(ctx context.Context, pool *pgxpool.Pool, schema, approvedBy string) (AccessCutoverReport, error) {
	report := AccessCutoverReport{Changes: []AccessChange{}, Notes: []AccessNote{}}
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
				report.Changes = append(report.Changes, AccessChange{MerchantID: merchant, CustomerID: c.CustomerID, Entitlement: c.Entitlement, Change: c.Change,
					Approved: approved[[3]string{c.CustomerID.String(), c.Entitlement, c.Change}]})
			}
			for _, n := range notes {
				report.Notes = append(report.Notes, AccessNote{MerchantID: merchant, CustomerID: n.CustomerID, Note: n.Note, SourceType: n.SourceType, SourceID: n.SourceID, Entitlements: n.Entitlements})
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
		if err := q.ApproveAccessCutoverChange(ctx, gen.ApproveAccessCutoverChangeParams{MerchantID: c.MerchantID, CustomerID: c.CustomerID, Entitlement: c.Entitlement, Change: c.Change, ApprovedBy: approvedBy}); err != nil {
			return report, err
		}
		report.Changes[i].Approved = true
	}
	return report, nil
}
