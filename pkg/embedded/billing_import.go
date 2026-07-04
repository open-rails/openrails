package embedded

import (
	"context"

	"github.com/open-rails/openrails/internal/billingimport"
)

// #737: the DeclaredBilling import seam — a host hands over its legacy billing
// book as FACTS and OpenRails classifies them through the same decider pipeline
// the pull/probe/webhook planes use, evaluated at the declared AsOf horizon.
// The implementation (and the POST /v1/import/billing wire shape — the JSON
// tags on these types) lives in internal/billingimport so the HTTP handlers
// share it without an import cycle; these aliases are the stable embedded
// vocabulary.
type (
	DeclaredCustomer      = billingimport.DeclaredCustomer
	DeclaredPaymentMethod = billingimport.DeclaredPaymentMethod
	PaymentMethodRef      = billingimport.PaymentMethodRef
	CancelEvidence        = billingimport.CancelEvidence
	DunningEvidence       = billingimport.DunningEvidence
	DeclaredTransaction   = billingimport.DeclaredTransaction
	DeclaredSubscription  = billingimport.DeclaredSubscription
	DeclaredBilling       = billingimport.DeclaredBilling

	// BillingImportOptions configures ImportBilling.
	BillingImportOptions = billingimport.Options
	// BillingImportResult reports per-SourceID outcomes.
	BillingImportResult = billingimport.Result
)

// ImportBilling lands a host-declared billing book — see billingimport.Import.
func ImportBilling(ctx context.Context, opts BillingImportOptions) (BillingImportResult, error) {
	return billingimport.Import(ctx, opts)
}
