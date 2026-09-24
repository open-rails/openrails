package ledger_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// sqlVocabulary returns the text literals of the LAST matching definition in
// migration order: a later migration may drop and re-add a constraint.
func sqlVocabulary(t *testing.T, schema, pattern string) []string {
	t.Helper()
	all := regexp.MustCompile(`(?s)`+pattern+`.*?ANY \(ARRAY\[(.*?)\]\)`).FindAllStringSubmatch(schema, -1)
	require.NotEmpty(t, all, "migrations do not define %s", pattern)
	var out []string
	for _, lit := range regexp.MustCompile(`'([a-z_]+)'::text`).FindAllStringSubmatch(all[len(all)-1][1], -1) {
		out = append(out, lit[1])
	}
	sort.Strings(out)
	return out
}

func sorted[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

// #832: idx_ledger_transfers_lot_once is partial on named transfer_type
// literals, so Go and DB vocabularies must agree exactly or a typo silently
// escapes the lot-once index and a lot is deposited/revoked twice.
func TestLedgerVocabularyMatchesSchema(t *testing.T) {
	entries, err := postgresmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		c, err := postgresmigrations.FS.ReadFile(n)
		require.NoError(t, err)
		b.Write(c)
		b.WriteString("\n")
	}
	schema := b.String()

	require.Equal(t, sqlVocabulary(t, schema, `CONSTRAINT ledger_transfers_type_check CHECK`), sorted(ledger.AllTransferTypes))
	require.Equal(t, sqlVocabulary(t, schema, `CREATE UNIQUE INDEX idx_ledger_transfers_lot_once `), sorted(ledger.LotOnceTransferTypes))
	require.Subset(t, sorted(ledger.AllTransferTypes), sorted(ledger.LotOnceTransferTypes))
	require.Equal(t, sqlVocabulary(t, schema, `CONSTRAINT ledger_accounts_type_check CHECK`), sorted([]ledger.AccountType{
		ledger.CustomerBalance, ledger.PlatformRevenue, ledger.RailClearing, ledger.ArrearsLiability,
		ledger.ExpiredCredits, ledger.RevokedCredits, ledger.FXLiquidity, ledger.World,
	}))
}

// LED-12: a partial idempotency coordinate is refused.
func TestCoordRequiresEveryPart(t *testing.T) {
	require.NoError(t, ledger.Coord{Operation: ledger.OpSpend, Source: "invoke", SourceID: "r-1"}.Validate())
	require.NoError(t, ledger.Coord{Operation: ledger.UsageOperation("api.call"), Source: "invoke", SourceID: "r-1"}.Validate())
	for _, c := range []ledger.Coord{
		{Source: "invoke", SourceID: "r-1"},
		{Operation: " ", Source: "invoke", SourceID: "r-1"},
		{Operation: ledger.UsageOperation("  "), Source: "invoke", SourceID: "r-1"},
		{Operation: ledger.OpSpend, SourceID: "r-1"},
		{Operation: ledger.OpSpend, Source: "invoke", SourceID: " "},
	} {
		require.Error(t, c.Validate(), c.String())
	}
}
