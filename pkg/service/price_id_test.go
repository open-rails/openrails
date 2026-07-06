package service

import (
	"testing"

	"github.com/google/uuid"
)

func pInt(i int) *int     { return &i }
func pI64(i int64) *int64 { return &i }

// TestPriceDeterministicID pins the #662 price-id derivation: it is a pure,
// stable function of the immutable financial tuple (the
// unique_prices_product_amount_window columns), canonicalized so equal terms
// always hash equal — and every one of those columns participates, so a change
// to any of them (a reprice) mints a different id.
func TestPriceDeterministicID(t *testing.T) {
	pid := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	base := priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, pI64(0), pInt(72))

	// Stable: same tuple → same id (cross-process/cross-DB reproducibility).
	if base != priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, pI64(0), pInt(72)) {
		t.Fatal("same tuple must derive the same id")
	}
	// Currency is canonicalized (lowercased) like the unique index, so case is irrelevant.
	if base != priceDeterministicID(pid, 1_000_000, "USD", pInt(720), true, pI64(0), pInt(72)) {
		t.Fatal("currency case must not change the id")
	}

	// Every frozen column participates — each change hashes to a new id.
	sensitive := map[string]uuid.UUID{
		"product":    priceDeterministicID(uuid.MustParse("22222222-2222-4222-8222-222222222222"), 1_000_000, "usd", pInt(720), true, pI64(0), pInt(72)),
		"amount":     priceDeterministicID(pid, 2_000_000, "usd", pInt(720), true, pI64(0), pInt(72)),
		"currency":   priceDeterministicID(pid, 1_000_000, "eur", pInt(720), true, pI64(0), pInt(72)),
		"access":     priceDeterministicID(pid, 1_000_000, "usd", pInt(1), true, pI64(0), pInt(72)),
		"auto_renew": priceDeterministicID(pid, 1_000_000, "usd", pInt(720), false, pI64(0), pInt(72)),
		"trial_amt":  priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, pI64(500), pInt(72)),
		"trial_dur":  priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, pI64(0), pInt(24)),
	}
	for field, id := range sensitive {
		if id == base {
			t.Fatalf("changing %s must change the id", field)
		}
	}

	// NULL is a distinct value from 0 (the unique index is NULLS NOT DISTINCT,
	// so an absent nullable field must hash differently from a present zero).
	nilTrial := priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, nil, nil)
	zeroTrial := priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, pI64(0), pInt(0))
	if nilTrial == zeroTrial {
		t.Fatal("nil trial fields must derive a different id than zero trial fields")
	}
	// ...but two prices that both leave the trial fields NULL hash equal (NULLs
	// compare equal under the index), so a re-seed of the same flat price is stable.
	if nilTrial != priceDeterministicID(pid, 1_000_000, "usd", pInt(720), true, nil, nil) {
		t.Fatal("two NULL-trial prices with an equal tuple must derive the same id")
	}
}
