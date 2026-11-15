package reconcile

import (
	"context"

	log "github.com/sirupsen/logrus"
)

// confirmPaymentMethodFindings re-judges each vault's payment-method findings
// against a fresh read of that vault. A vault whose re-read fails keeps no
// finding this pass; the next pass judges it again.
func confirmPaymentMethodFindings(ctx context.Context, provider Provider, fetcher RailFetcher, local *LocalState, findings []Finding) []Finding {
	if k, ok := fetcher.(keyedFetcher); ok {
		fetcher = k.RailFetcher
	}
	confirmer, ok := fetcher.(*NMIFetcher)
	if !ok {
		return findings
	}
	vaults := map[string]bool{}
	for i := range findings {
		if findings[i].Type == FindingPaymentMethodMismatch {
			vaults[findings[i].SubjectKey] = true
		}
	}
	if len(vaults) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	for i := range findings {
		if findings[i].Type != FindingPaymentMethodMismatch {
			out = append(out, findings[i])
		}
	}
	traits := traitsFor(provider)
	for vault := range vaults {
		fresh, err := confirmer.ConfirmPaymentMethods(ctx, vault)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("vault_id", vault).Warn("reconcile: vault re-read failed; payment-method finding deferred to the next pass")
			continue
		}
		scoped := &LocalState{}
		for j := range local.PaymentMethods {
			if local.PaymentMethods[j].RailCustomerRef == vault {
				scoped.PaymentMethods = append(scoped.PaymentMethods, local.PaymentMethods[j])
			}
		}
		// The re-read is exhaustive for this one vault.
		traits.paymentMethodsExhaustive = true
		out = append(out, diffPaymentMethods(provider, scoped, buildRemoteIndex(&RemoteSnapshot{PaymentMethods: fresh}), traits)...)
	}
	return out
}
