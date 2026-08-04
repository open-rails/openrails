package merchants

import (
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

// TestSecretNameRejectsTraversal is the SEC-24 item 6 guard. cleanSecretName's
// output is joined into a Vault path. Every caller allowlists the name today
// and accountID is PathEscaped, so this is not reachable — the point is that
// the guard should not depend on that staying true.
func TestSecretNameRejectsTraversal(t *testing.T) {
	mid := merchant.ID(uuid.New())

	traversals := []string{
		"..",
		"../../root",
		"psps/../../other-merchant/nmi/security_key",
		"stripe/../../..",
		"./stripe/secret_key",
		"stripe/./secret_key",
		"/../",
	}
	for _, name := range traversals {
		if got := cleanSecretName(name); got != "" {
			t.Errorf("cleanSecretName(%q) = %q — a traversal segment must be rejected, "+
				"the result is joined into a Vault path (SEC-24 item 6)", name, got)
		}
		if err := validateSecretRef(mid, name); err == nil {
			t.Errorf("validateSecretRef(%q) accepted a traversal name", name)
		}
	}

	// Ordinary names still work.
	for _, name := range []string{
		SecretStripeSecretKey,
		SecretNMISecurityKey,
		"psps/nmi/production/579145/security_key",
	} {
		if got := cleanSecretName(name); got == "" {
			t.Errorf("cleanSecretName(%q) rejected a legitimate name", name)
		}
		if err := validateSecretRef(mid, name); err != nil {
			t.Errorf("validateSecretRef(%q) rejected a legitimate name: %v", name, err)
		}
	}
}
