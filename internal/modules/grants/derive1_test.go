package grants

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// #631 derive-1 pure window/spec math — no DB.
func TestSubscriptionWindow(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	start := t0
	periodEnd := t0.Add(720 * time.Hour) // +30d
	ended := t0.Add(100 * time.Hour)
	ptr := func(x time.Time) *time.Time { return &x }

	cases := []struct {
		name               string
		row                gen.ListUngrantedSubscriptionsRow
		wantOK             bool
		wantStart, wantEnd time.Time
	}{
		{
			name:      "period bounds preferred",
			row:       gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: ptr(start), CurrentPeriodEndsAt: ptr(periodEnd)},
			wantOK:    true,
			wantStart: start, wantEnd: periodEnd,
		},
		{
			name:      "fallback to started_at + ended_at",
			row:       gen.ListUngrantedSubscriptionsRow{StartedAt: start, EndedAt: ptr(ended)},
			wantOK:    true,
			wantStart: start, wantEnd: ended,
		},
		{
			name:   "no end → invalid",
			row:    gen.ListUngrantedSubscriptionsRow{StartedAt: start},
			wantOK: false,
		},
		{
			name:   "end not after start → invalid",
			row:    gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: ptr(periodEnd), CurrentPeriodEndsAt: ptr(start)},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStart, gotEnd, ok := subscriptionWindow(c.row)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if !gotStart.Equal(c.wantStart) || !gotEnd.Equal(c.wantEnd) {
				t.Fatalf("window = [%s,%s), want [%s,%s)", gotStart, gotEnd, c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestProductSpecKeys(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"two features sorted", `{"premium": 720, "vip": null}`, []string{"premium", "vip"}},
		{"empty map", `{}`, []string{}},
		{"trims + drops blanks", `{"  premium  ": 1, "": 2}`, []string{"premium"}},
		{"invalid json → nil", `not-json`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := productSpecKeys([]byte(c.raw))
			if len(got) != len(c.want) {
				t.Fatalf("keys = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("keys = %v, want %v", got, c.want)
				}
			}
		})
	}
	// nil bytes → nil, no panic.
	if productSpecKeys(nil) != nil {
		t.Fatal("nil raw should yield nil")
	}
}

// guard: a payment-linked derive sets Payment so the refund check sees it.
func TestDeriveWalletGrant_RejectsBadWindow(t *testing.T) {
	l := &Ledger{merchant: uuid.New()}
	// expires == purchased → not after → no-op, no panic, no DB touch.
	now := time.Now().UTC()
	row := gen.ListUngrantedWalletPaymentsRow{ID: uuid.New(), CustomerID: uuid.New(), PurchasedAt: now, ExpiresAt: now}
	if err := l.DeriveWalletGrant(nil, row); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}
