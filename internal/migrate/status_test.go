package migrate

import (
	"testing"

	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

func TestBuildPostgresStatusRequiresAnExactVerifiableLedger(t *testing.T) {
	migrations := []migratekit.Migration{
		{Name: "0001_one.up.sql", Content: "SELECT 1;"},
		{Name: "0002_two.up.sql", Content: "SELECT 2;"},
		{Name: "0003_three.up.sql", Content: "SELECT 3;"},
		{Name: "0004_four.up.sql", Content: "SELECT 4;"},
		{Name: "0005_five.up.sql", Content: "SELECT 5;"},
	}
	exactRecord := func(migration migratekit.Migration) migratekit.AppliedRecord {
		return migratekit.AppliedRecord{
			Key:            migratekit.Prefix(migration.Name),
			Filename:       migration.Name,
			Digest:         migratekit.ContentDigest(migration.Content),
			SemanticDigest: migratekit.SemanticContentDigest(migration.Content),
			Status:         "applied",
		}
	}

	t.Run("exact", func(t *testing.T) {
		applied := make([]migratekit.AppliedRecord, 0, len(migrations))
		for _, migration := range migrations {
			applied = append(applied, exactRecord(migration))
		}
		report := buildPostgresStatus(migratekit.Status{
			App: "openrails", Schema: "openrails", Applied: applied,
		}, migrations)
		require.True(t, report.Exact)
		require.Empty(t, report.Missing)
		require.Empty(t, report.Orphaned)
		require.Empty(t, report.Drift)
	})

	t.Run("every non-exact state", func(t *testing.T) {
		filenameMismatch := exactRecord(migrations[1])
		filenameMismatch.Filename = "0002_other.up.sql"
		filenameMismatch.Digest = "wrong-content"
		filenameMismatch.SemanticDigest = "wrong-semantic"
		unfinished := exactRecord(migrations[3])
		unfinished.Status = "failed"
		report := buildPostgresStatus(migratekit.Status{
			App: "openrails", Schema: "openrails",
			Applied: []migratekit.AppliedRecord{
				exactRecord(migrations[0]),
				filenameMismatch,
				{Key: "3"},
				unfinished,
				{Key: "6", Filename: "0006_orphan.up.sql", Digest: "orphan", Status: "applied"},
			},
		}, migrations)

		require.False(t, report.Exact)
		require.Equal(t, []EmbeddedMigration{report.Embedded[4]}, report.Missing)
		require.Equal(t, "6", report.Orphaned[0].Key)
		kinds := make(map[string]bool, len(report.Drift))
		for _, drift := range report.Drift {
			kinds[drift.Kind] = true
		}
		for _, kind := range []string{
			"filename_mismatch",
			"content_hash_mismatch",
			"semantic_hash_mismatch",
			"unverifiable_legacy_row",
			"unfinished",
		} {
			require.True(t, kinds[kind], "missing drift kind %s", kind)
		}
		require.Contains(t, report.Report(), "exact: false")
		require.Contains(t, report.Report(), "ORPHANED")
		require.Contains(t, report.Report(), "DRIFT")
	})
}
