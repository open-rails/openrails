package intents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderWritesStayBehindIntents is the #674 enforcement guard: provider
// WRITE choke points (NMI money movers / recurring mutations / vault deletes,
// the Solana sign-and-submit path) may only be called from an explicitly
// allowlisted set of files — the intent handlers plus the documented
// not-yet-migrated sites. Adding a NEW call site fails CI instead of review.
//
// This is the cheap, honest, TEXTUAL version. Its limits:
//   - it matches method-call tokens, so a wrapper, method value, or renamed
//     import that reaches the same wire call is invisible to it;
//   - allowlisted files are trusted wholesale (a second call added to an
//     allowlisted file passes);
//   - Stripe writes are NOT scanned here — they are architecturally choked
//     through internal/integrations/stripeapi (readonly transport blocks
//     writes), which is the stronger guard;
//   - internal/integrations/** is excluded: that layer IS the client.
func TestProviderWritesStayBehindIntents(t *testing.T) {
	root := filepath.Join("..", "..")

	// token -> allowed files (relative to repo root). Keep entries commented
	// with WHY they are allowed.
	allowed := map[string]map[string]string{
		".RunSale(": {
			"internal/modules/checkout/nmi_sale_intent.go":         "nmi_sale intent handler (the sanctioned executor)",
			"internal/modules/checkout/service.go":                 "upgrade proration: order id content-derived + pre/post verify (#674); full intent migration deferred",
			"internal/modules/payments/rails/nmidirect/charger.go": "#297 charge-seam implementation: the ONE seam wire call, reached only via the money CollectionAdapter choke (topup_charge intent handler + arrears/invoice worker attempt-count keys, #672/#673)",
		},
		".AddRecurringSubscription(": {
			"internal/modules/checkout/nmi_subscription_intent.go": "nmi_subscription_create intent handler",
			"internal/modules/checkout/service.go":                 "upgrade successor create: content-derived order id + ambiguity ⇒ roster-scan adopt-or-processing (#674 tail); full saga assessed and declined (completed.md #674)",
		},
		".AttemptManualRebill(": {
			"internal/intents/manual_rebill.go": "manual_rebill intent handler",
		},
		// Any NMI refund must build nmi.RefundParams — scanning the params
		// constructor avoids false positives on PaymentService.Refund (a local
		// payment-row write, not a provider call).
		"nmi.RefundParams{": {
			"internal/intents/refund.go":           "nmi_refund intent handler",
			"internal/modules/checkout/service.go": "upgrade compensation refund; intent migration deferred",
		},
		".Void(": {},
		".DeleteRecurringSubscription(": {
			"internal/intents/nmi_delete.go":                  "nmi_delete_subscription intent handler",
			"internal/modules/checkout/service.go":            "upgrade/cancel rollback of a just-created remote sub (reactive compensation)",
			"internal/modules/subscriptions/admin_service.go": "reactive admin cancel; deferred deletes route through intents, immediate ones are user/admin-reactive",
			"internal/modules/subscriptions/user_service.go":  "reactive user cancel (see admin_service note)",
		},
		".UpdateSubscriptionPaymentSource(": {
			"internal/intents/nmi_payment_source_update.go": "nmi_payment_source_update intent handler (the sanctioned executor; both call sites route through PaymentSourceUpdateThrough)",
		},
		".DeleteCustomerVault(": {
			"internal/intents/nmi_vault_delete.go":             "nmi_vault_delete intent handler (the sanctioned executor for durable user-initiated deletes)",
			"internal/modules/paymentmethods/vault_service.go": "reactive decline-cleanup only (CreateVault local-store-failure cleanup + CleanupVaultBestEffort): vault referenced nowhere, harmless if lost; durable deletes route through DeleteVault → nmi_vault_delete intent (#674 tail)",
		},
		".DeleteCustomerBillingEntry(": {
			"internal/intents/nmi_vault_delete.go":             "nmi_vault_delete intent handler (shared-vault #682 billing-entry-scoped delete)",
			"internal/modules/paymentmethods/vault_service.go": "reactive decline-cleanup path (CleanupVaultBestEffort shared-vault scope); see .DeleteCustomerVault( note",
		},
		"solanaint.BuildSignSubmit": {
			"internal/modules/solana/recurring/plan_service.go": "the Submitter implementation — the single Solana sign+submit choke; pulls route through the solana_pull intent handler",
		},
	}

	violations := []string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel == "internal/integrations",
				strings.HasPrefix(rel, "internal/integrations/"),
				strings.HasPrefix(rel, ".git"),
				strings.HasPrefix(rel, "node_modules"),
				strings.HasPrefix(rel, "vendor"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "pkg/") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)
		for token, files := range allowed {
			if !strings.Contains(src, token) {
				continue
			}
			if _, ok := files[rel]; !ok {
				violations = append(violations,
					rel+" calls provider-write choke point "+token+
						" — route it through a provider intent (internal/intents, #674) or add an allowlist entry WITH justification")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}
