package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/migratekit"

	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// ErrMigrationStatusDrift means the applied ledger is not an exact, verifiable
// match for the migrations embedded in this build.
var ErrMigrationStatusDrift = errors.New("openrails migration status is not exact")

// EmbeddedMigration is one migration compiled into the current binary.
type EmbeddedMigration struct {
	Key            string `json:"key"`
	Filename       string `json:"filename"`
	ContentSHA256  string `json:"content_sha256"`
	SemanticSHA256 string `json:"semantic_sha256"`
}

// AppliedMigration is one OpenRails row from migratekit's Postgres ledger.
type AppliedMigration struct {
	Key            string `json:"key"`
	Filename       string `json:"filename"`
	ContentSHA256  string `json:"content_sha256"`
	SemanticSHA256 string `json:"semantic_sha256"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
}

// MigrationDrift describes one identity, state, or digest mismatch.
type MigrationDrift struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Filename string `json:"filename,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// PostgresStatus compares the embedded OpenRails chain with one database.
type PostgresStatus struct {
	App      string              `json:"app"`
	Schema   string              `json:"schema"`
	Exact    bool                `json:"exact"`
	Embedded []EmbeddedMigration `json:"embedded"`
	Applied  []AppliedMigration  `json:"applied"`
	Missing  []EmbeddedMigration `json:"missing"`
	Orphaned []AppliedMigration  `json:"orphaned"`
	Drift    []MigrationDrift    `json:"drift"`
}

// InspectPostgres returns a deployment-grade exactness report for OpenRails'
// effective Postgres schema.
func InspectPostgres(ctx context.Context, cfg *config.Config) (report PostgresStatus, err error) {
	if cfg == nil || cfg.DB == nil {
		return report, fmt.Errorf("missing database config")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	if err != nil {
		return report, fmt.Errorf("load openrails migrations: %w", err)
	}
	schema := cfg.DB.SchemaName()
	migrations = rewriteMigrationsSchema(migrations, schema)

	sqlDB, err := sql.Open("pgx", cfg.DB.GetConnectionString())
	if err != nil {
		return report, fmt.Errorf("open postgres: %w", err)
	}
	defer func() { err = errors.Join(err, sqlDB.Close()) }()

	status, err := migratekit.NewPostgres(sqlDB, config.MigratekitApp).
		WithSchema(schema).
		Status(ctx, migrations)
	if err != nil {
		return report, fmt.Errorf("inspect openrails migrations: %w", err)
	}
	return buildPostgresStatus(status, migrations), nil
}

func buildPostgresStatus(status migratekit.Status, migrations []migratekit.Migration) PostgresStatus {
	report := PostgresStatus{
		App:      status.App,
		Schema:   status.Schema,
		Embedded: make([]EmbeddedMigration, 0, len(migrations)),
		Applied:  make([]AppliedMigration, 0, len(status.Applied)),
		Missing:  []EmbeddedMigration{},
		Orphaned: []AppliedMigration{},
		Drift:    []MigrationDrift{},
	}
	embeddedByKey := make(map[string]EmbeddedMigration, len(migrations))
	for _, migration := range migrations {
		embedded := EmbeddedMigration{
			Key:            migratekit.Prefix(migration.Name),
			Filename:       migration.Name,
			ContentSHA256:  migratekit.ContentDigest(migration.Content),
			SemanticSHA256: migratekit.SemanticContentDigest(migration.Content),
		}
		report.Embedded = append(report.Embedded, embedded)
		embeddedByKey[embedded.Key] = embedded
	}

	appliedByKey := make(map[string]migratekit.AppliedRecord, len(status.Applied))
	for _, record := range status.Applied {
		appliedByKey[record.Key] = record
		applied := appliedMigration(record)
		report.Applied = append(report.Applied, applied)

		embedded, exists := embeddedByKey[record.Key]
		if !exists {
			report.Orphaned = append(report.Orphaned, applied)
			continue
		}
		if record.Status != "" && record.Status != "applied" {
			report.Drift = append(report.Drift, MigrationDrift{
				Kind: "unfinished", Key: record.Key, Filename: embedded.Filename,
				Expected: "applied", Actual: record.Status,
			})
		}
		if record.Filename == "" || record.Digest == "" {
			report.Drift = append(report.Drift, MigrationDrift{
				Kind: "unverifiable_legacy_row", Key: record.Key, Filename: embedded.Filename,
				Expected: "recorded filename and content_sha256",
			})
			continue
		}
		if record.Filename != embedded.Filename {
			report.Drift = append(report.Drift, MigrationDrift{
				Kind: "filename_mismatch", Key: record.Key, Filename: embedded.Filename,
				Expected: embedded.Filename, Actual: record.Filename,
			})
		}
		if record.Digest != embedded.ContentSHA256 {
			report.Drift = append(report.Drift, MigrationDrift{
				Kind: "content_hash_mismatch", Key: record.Key, Filename: embedded.Filename,
				Expected: embedded.ContentSHA256, Actual: record.Digest,
			})
		}
		if record.SemanticDigest != "" && record.SemanticDigest != embedded.SemanticSHA256 {
			report.Drift = append(report.Drift, MigrationDrift{
				Kind: "semantic_hash_mismatch", Key: record.Key, Filename: embedded.Filename,
				Expected: embedded.SemanticSHA256, Actual: record.SemanticDigest,
			})
		}
	}

	for _, embedded := range report.Embedded {
		if _, exists := appliedByKey[embedded.Key]; !exists {
			report.Missing = append(report.Missing, embedded)
		}
	}
	report.Exact = len(report.Missing) == 0 && len(report.Orphaned) == 0 && len(report.Drift) == 0
	return report
}

func appliedMigration(record migratekit.AppliedRecord) AppliedMigration {
	return AppliedMigration{
		Key:            record.Key,
		Filename:       record.Filename,
		ContentSHA256:  record.Digest,
		SemanticSHA256: record.SemanticDigest,
		Status:         record.Status,
		Error:          record.Error,
	}
}

// Report renders the exactness report for a human operator.
func (s PostgresStatus) Report() string {
	var out strings.Builder
	fmt.Fprintf(&out, "openrails migration status — app %s (schema %s)\n", s.App, s.Schema)
	fmt.Fprintf(&out, "  exact: %t    embedded: %d    applied: %d    missing: %d    orphaned: %d    drift: %d\n",
		s.Exact, len(s.Embedded), len(s.Applied), len(s.Missing), len(s.Orphaned), len(s.Drift))

	writeEmbeddedSection(&out, "EMBEDDED", s.Embedded)
	writeAppliedSection(&out, "APPLIED", s.Applied)
	writeEmbeddedSection(&out, "MISSING", s.Missing)
	writeAppliedSection(&out, "ORPHANED", s.Orphaned)
	if len(s.Drift) > 0 {
		out.WriteString("\nDRIFT\n")
		for _, drift := range s.Drift {
			fmt.Fprintf(&out, "  %s key=%s file=%s expected=%s actual=%s\n",
				drift.Kind, drift.Key, drift.Filename, drift.Expected, drift.Actual)
		}
	}
	return out.String()
}

func writeEmbeddedSection(out *strings.Builder, heading string, migrations []EmbeddedMigration) {
	out.WriteString("\n" + heading + "\n")
	if len(migrations) == 0 {
		out.WriteString("  none\n")
		return
	}
	for _, migration := range migrations {
		fmt.Fprintf(out, "  %s key=%s content_sha256=%s semantic_sha256=%s\n",
			migration.Filename, migration.Key, migration.ContentSHA256, migration.SemanticSHA256)
	}
}

func writeAppliedSection(out *strings.Builder, heading string, migrations []AppliedMigration) {
	out.WriteString("\n" + heading + "\n")
	if len(migrations) == 0 {
		out.WriteString("  none\n")
		return
	}
	for _, migration := range migrations {
		fmt.Fprintf(out, "  %s key=%s status=%s content_sha256=%s semantic_sha256=%s\n",
			migration.Filename, migration.Key, migration.Status, migration.ContentSHA256, migration.SemanticSHA256)
	}
}
