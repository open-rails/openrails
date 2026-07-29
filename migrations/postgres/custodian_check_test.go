package postgresmigrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestCustodianColumnIsStated (or#880 phase 1) guards the custody axis in the
// migration set itself: payment_methods.custodian must be renamed from
// vault_provider, backfilled off the empty string, defaulted to 'psp' and
// pinned by a CHECK. The point of the CHECK is that "who holds this card" can
// never again be an absence.
//
// Static test over the embedded migration files; apply-time correctness is
// covered by internal/modules/paymentmethods (custodian_integration_test.go).
func TestCustodianColumnIsStated(t *testing.T) {
	files, err := fs.Glob(FS, "0025_*.up.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("expected exactly one 0025 up migration, got %v (err %v)", files, err)
	}
	content, err := fs.ReadFile(FS, files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	sql := collapseWS(string(content))

	for _, want := range []string{
		"rename column vault_provider to custodian",
		"set custodian = 'psp' where custodian = ''",
		"alter column custodian set default 'psp'",
		"payment_methods_custodian_check",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s: expected %q", files[0], want)
		}
	}

	// No later migration may relax the constraint or reinstate the old name.
	all, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, f := range all {
		if f <= files[0] {
			continue
		}
		body, err := fs.ReadFile(FS, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := collapseWS(string(body))
		if strings.Contains(s, "drop constraint payment_methods_custodian_check") ||
			strings.Contains(s, "drop constraint if exists payment_methods_custodian_check") {
			t.Errorf("%s: payment_methods_custodian_check must not be dropped — an unstated custodian would become possible again", f)
		}
		if strings.Contains(s, "payment_methods") && strings.Contains(s, "vault_provider") {
			t.Errorf("%s: vault_provider is retired (or#880) — the column is custodian", f)
		}
	}
}
