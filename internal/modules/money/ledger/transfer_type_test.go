package ledger_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/modules/money/ledger"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
)

// #832: transfer_type used to be free text while account_type had a closed-set
// CHECK. idx_ledger_transfers_lot_once — what stops a credit lot being
// deposited, expired or revoked twice — is PARTIAL on three named transfer_type
// literals, so a typo in a new type fell outside it and the duplicate posted.
// The DB vocabulary and the Go constants must therefore agree EXACTLY; this
// pins both against the migrations themselves.

var (
	reTypeCheck = regexp.MustCompile(`(?s)CONSTRAINT ledger_transfers_type_check CHECK \(\(transfer_type = ANY \(ARRAY\[(.*?)\]\)\)\)`)
	reLotOnce   = regexp.MustCompile(`(?s)CREATE UNIQUE INDEX idx_ledger_transfers_lot_once .*?transfer_type = ANY \(ARRAY\[(.*?)\]\)`)
	reSQLText   = regexp.MustCompile(`'([a-z_]+)'::text`)
)

func allMigrations(t *testing.T) string {
	t.Helper()
	entries, err := postgresmigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
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
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		b.Write(c)
		b.WriteString("\n")
	}
	return b.String()
}

func sqlLiterals(t *testing.T, re *regexp.Regexp, schema, what string) []string {
	t.Helper()
	m := re.FindStringSubmatch(schema)
	if m == nil {
		t.Fatalf("migrations do not define %s", what)
	}
	var out []string
	for _, lit := range reSQLText.FindAllStringSubmatch(m[1], -1) {
		out = append(out, lit[1])
	}
	sort.Strings(out)
	return out
}

func goVocabulary(ts []ledger.TransferType) []string {
	out := make([]string, len(ts))
	for i, v := range ts {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

func TestTransferTypeVocabularyMatchesSchema(t *testing.T) {
	schema := allMigrations(t)

	db := sqlLiterals(t, reTypeCheck, schema, "ledger_transfers_type_check")
	if got, want := strings.Join(goVocabulary(ledger.AllTransferTypes), ","), strings.Join(db, ","); got != want {
		t.Errorf("ledger.AllTransferTypes = [%s], ledger_transfers_type_check = [%s] — the Go vocabulary and "+
			"the DB CHECK must agree exactly, or a value one side accepts is a runtime error on the other", got, want)
	}
}

// A lot-once type outside the CHECK vocabulary is the #832 hazard itself: the
// index silently covers nothing, and the lot can be deposited or revoked twice.
func TestLotOnceTransferTypesAreInTheVocabulary(t *testing.T) {
	schema := allMigrations(t)

	vocab := map[string]bool{}
	for _, v := range ledger.AllTransferTypes {
		vocab[string(v)] = true
	}
	for _, v := range sqlLiterals(t, reLotOnce, schema, "idx_ledger_transfers_lot_once") {
		if !vocab[v] {
			t.Errorf("idx_ledger_transfers_lot_once names transfer_type %q, which is not in the vocabulary — "+
				"the index covers no rows and the lot-once invariant is silently off", v)
		}
	}
	if got, want := strings.Join(sqlLiterals(t, reLotOnce, schema, "idx_ledger_transfers_lot_once"), ","),
		strings.Join(goVocabulary(ledger.LotOnceTransferTypes), ","); got != want {
		t.Errorf("idx_ledger_transfers_lot_once = [%s], ledger.LotOnceTransferTypes = [%s]", got, want)
	}
}
