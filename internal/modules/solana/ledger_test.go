package solana

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// One transfer's classification, in precedence order: nothing paid, already
// paid, late, closed checkout, short, excess.
func TestDecide(t *testing.T) {
	settle := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := settle.Add(d); return &v }
	pending := gen.OpenrailsSolanaPayReference{Status: ReferencePending, SettleUntil: settle}
	paid := gen.OpenrailsSolanaPayReference{Status: ReferenceConfirmed, SettleUntil: settle}
	for _, tc := range []struct {
		name   string
		ref    gen.OpenrailsSolanaPayReference
		open   bool
		amount uint64
		landed *time.Time
		want   Disposition
		reason string
	}{
		{"exact", pending, true, 100, at(-time.Minute), Credited, ""},
		{"excess", pending, true, 101, at(-time.Minute), Credited, ReasonOverpaid},
		{"on the deadline", pending, true, 100, at(0), Credited, ""},
		{"nothing to the merchant", paid, false, 0, at(time.Hour), Ignored, ""},
		{"second transfer", paid, true, 100, at(-time.Minute), Review, ReasonAlreadyPaid},
		{"late", pending, true, 100, at(time.Second), Review, ReasonLate},
		{"no block time is judged now", pending, true, 100, nil, Review, ReasonLate},
		{"closed checkout", pending, false, 100, at(-time.Minute), Review, ReasonSessionClosed},
		{"short", pending, true, 99, at(-time.Minute), Review, ReasonUnderpaid},
	} {
		d, reason := Decide(tc.ref, tc.open, 100, ObservedTransfer{Amount: tc.amount, LandedAt: tc.landed}, settle.Add(time.Hour))
		require.Equal(t, tc.want, d, tc.name)
		require.Equal(t, tc.reason, reason, tc.name)
	}
}
