package solana

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// One transfer's classification, in precedence order: foreign, unreadable,
// wrong asset, nothing paid, no quote, already paid, late, closed checkout,
// short, excess.
func TestDecide(t *testing.T) {
	settle := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := settle.Add(d); return &v }
	pending := gen.BillingSolanaPayReference{Status: ReferencePending, SettleUntil: settle}
	paid := gen.BillingSolanaPayReference{Status: ReferenceConfirmed, SettleUntil: settle}
	for _, tc := range []struct {
		name    string
		ref     gen.BillingSolanaPayReference
		open    bool
		amount  uint64
		landed  *time.Time
		want    Disposition
		reason  string
		mutate  func(*ObservedTransfer)
		noQuote bool
	}{
		{name: "foreign", ref: pending, open: true, amount: 100, landed: at(0), want: Ignored, mutate: func(o *ObservedTransfer) { o.Foreign = true }},
		{name: "unreadable", ref: paid, open: true, landed: at(0), want: Review, reason: ReasonUnreadable, mutate: func(o *ObservedTransfer) { o.Unreadable = true }},
		{name: "wrong asset", ref: pending, open: true, landed: at(0), want: Review, reason: ReasonWrongAsset, mutate: func(o *ObservedTransfer) { o.Other = true }},
		{name: "no quote settles nothing", ref: pending, open: true, amount: 100, landed: at(0), want: Review, reason: ReasonSettleFailed, noQuote: true},
		{name: "exact", ref: pending, open: true, amount: 100, landed: at(-time.Minute), want: Credited, reason: ""},
		{name: "excess", ref: pending, open: true, amount: 101, landed: at(-time.Minute), want: Credited, reason: ReasonOverpaid},
		{name: "on the deadline", ref: pending, open: true, amount: 100, landed: at(0), want: Credited, reason: ""},
		{name: "nothing to the merchant", ref: paid, open: false, amount: 0, landed: at(time.Hour), want: Ignored, reason: ""},
		{name: "second transfer", ref: paid, open: true, amount: 100, landed: at(-time.Minute), want: Review, reason: ReasonAlreadyPaid},
		{name: "late", ref: pending, open: true, amount: 100, landed: at(time.Second), want: Review, reason: ReasonLate},
		{name: "no block time is judged now", ref: pending, open: true, amount: 100, landed: nil, want: Review, reason: ReasonLate},
		{name: "closed checkout", ref: pending, open: false, amount: 100, landed: at(-time.Minute), want: Review, reason: ReasonSessionClosed},
		{name: "short", ref: pending, open: true, amount: 99, landed: at(-time.Minute), want: Review, reason: ReasonUnderpaid},
	} {
		obs := ObservedTransfer{Amount: tc.amount, LandedAt: tc.landed}
		if tc.mutate != nil {
			tc.mutate(&obs)
		}
		expected := uint64(100)
		if tc.noQuote {
			expected = 0
		}
		d, reason := Decide(tc.ref, tc.open, expected, obs, settle.Add(time.Hour))
		require.Equal(t, tc.want, d, tc.name)
		require.Equal(t, tc.reason, reason, tc.name)
	}
}
