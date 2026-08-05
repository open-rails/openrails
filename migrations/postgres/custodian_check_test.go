package postgresmigrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestCustodianColumnIsStated (or#880) guards the custody axis in the schema
// itself: "who holds this card" is a stated column with a default and a CHECK,
// never an absence. It used to assert the RENAME that introduced it; the
// squashed baseline states the final shape directly, and the rename is gone.
//
// Static test over the embedded migration files; apply-time correctness is
// covered by internal/modules/paymentmethods (custodian_integration_test.go).
func TestCustodianColumnIsStated(t *testing.T) {
	sql := collapseWS(loadSchema001(t))

	for _, want := range []string{
		"custodian text default 'psp'::text not null",
		"constraint payment_methods_custodian_check check ((custodian = any (array['psp'::text, 'basis_theory'::text])))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("baseline: expected %q", want)
		}
	}

	// No migration may relax the constraint or reinstate the pre-or#880 name.
	all, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations found: this guard would pass vacuously")
	}
	for _, f := range all {
		body, err := fs.ReadFile(FS, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := collapseWS(string(body))
		if strings.Contains(s, "drop constraint payment_methods_custodian_check") {
			t.Errorf("%s: drops payment_methods_custodian_check — custody may not become an absence again (or#880)", f)
		}
		if strings.Contains(s, "vault_provider") {
			t.Errorf("%s: vault_provider is retired (or#880) — the column is custodian", f)
		}
	}
}

// TestVaultedCardRailIsRetired (or#879) guards the rail vocabulary.
// `vaulted_card` was never a gateway kind — it was NMI with the card held at
// Basis Theory — and the whole cost of the mistake was that every
// rail-dispatching switch had to remember to alias it. A migration that writes
// the value back would reintroduce rows no switch handles.
func TestVaultedCardRailIsRetired(t *testing.T) {
	all, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations found: this guard would pass vacuously")
	}
	for _, f := range all {
		body, err := fs.ReadFile(FS, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := collapseWS(string(body))
		if strings.Contains(s, "'vaulted_card'") {
			t.Errorf("%s: 'vaulted_card' is retired (or#879) — the rail is nmi and the custodian is basis_theory", f)
		}
		if strings.Contains(s, "payment_methods") && strings.Contains(s, "vault_fingerprint") {
			t.Errorf("%s: vault_fingerprint is retired (or#871) — the column is fingerprint", f)
		}
	}

	// The custody-scoped owner lookup survives the retirement: a card held by a
	// custodian still resolves to exactly one merchant.
	sql := collapseWS(loadSchema001(t))
	if !strings.Contains(sql, "function openrails.custodian_owner_by_identity") {
		t.Error("baseline: expected the custodian_owner_by_identity lookup (or#880)")
	}
	if !strings.Contains(sql, "fingerprint text default ''::text not null") {
		t.Error("baseline: expected payment_methods.fingerprint (or#871)")
	}
}
