package books

import "slices"

// Basis is how a provider's money figure is known.
type Basis string

const (
	// Exact is the provider's own figure, or its statement's.
	Exact Basis = "exact"
	// Derived is inferred from exact figures, such as fees from what a PSP
	// deposited against what it took.
	Derived Basis = "derived"
	// Estimated comes from agreed terms. The exact figure replaces it: the
	// estimate is posted back at its own coordinate and the figure posted.
	Estimated Basis = "estimated"
)

// The coordinate sources of provider figures.
const (
	// StripeBalanceTransaction is a Stripe balance transaction (txn_).
	StripeBalanceTransaction = "stripe_balance_transaction"
	// StripePayout is a Stripe payout (po_).
	StripePayout = "stripe_payout"
)

// sources are the provider figures' coordinate sources and their basis.
var sources = map[string]Basis{
	StripeBalanceTransaction: Exact,
	StripePayout:             Exact,
}

// providerFigures are the transfer types that record a figure a provider
// reports; each posts at a source of known basis.
var providerFigures = []TransferType{ProcessorFee, ChargebackFee, PayoutSent, CashDeposited, ReserveHeld, ReserveReleased}

// SourceBasis is the basis of the provider figures posted at a coordinate
// source.
func SourceBasis(source string) (Basis, bool) {
	b, ok := sources[source]
	return b, ok
}

// ProviderFigure reports whether a transfer type records a provider's figure.
func ProviderFigure(t TransferType) bool { return slices.Contains(providerFigures, t) }
