package abusestate

import (
	"log/slog"
	"testing"
	"time"
)

func TestClaimTakesOneSlotUntilItOpens(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler), nil)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if next, ok := s.Claim(t.Context(), "slot", now, now.Add(time.Hour)); !ok || !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("first claim = %v, %v", next, ok)
	}
	if next, ok := s.Claim(t.Context(), "slot", now.Add(59*time.Minute), now.Add(2*time.Hour)); ok || !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("claim inside the slot = %v, %v; want it held until %v", next, ok, now.Add(time.Hour))
	}
	if _, ok := s.Claim(t.Context(), "other", now, now.Add(time.Hour)); !ok {
		t.Fatal("another key's slot is its own")
	}
	if next, ok := s.Claim(t.Context(), "slot", now.Add(time.Hour), now.Add(2*time.Hour)); !ok || !next.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("claim once open = %v, %v", next, ok)
	}
}

func TestReopenOpensAHeldSlot(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler), nil)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, ok := s.Claim(t.Context(), "slot", now, now.Add(time.Hour)); !ok {
		t.Fatal("first claim refused")
	}
	s.Reopen(t.Context(), "slot")
	if _, ok := s.Claim(t.Context(), "slot", now.Add(time.Minute), now.Add(time.Hour)); !ok {
		t.Fatal("a reopened slot stayed held")
	}
}

// A slot kept to the millisecond never opens before the instant claimed.
func TestClaimNeverOpensEarly(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler), nil)
	now := time.Date(2026, 11, 20, 10, 0, 0, 123_456_789, time.UTC)
	next := now.Add(time.Hour)
	opens, ok := s.Claim(t.Context(), "slot", now, next)
	if !ok || opens.Before(next) {
		t.Fatalf("claim = %v, %v; want it kept until %v", opens, ok, next)
	}
	if at, ok := s.Claim(t.Context(), "slot", next.Truncate(time.Millisecond), next.Add(time.Hour)); ok || at.Before(next) {
		t.Fatalf("a claim a fraction of a millisecond before %v = %v, %v", next, at, ok)
	}
	if _, ok := s.Claim(t.Context(), "slot", opens, opens.Add(time.Hour)); !ok {
		t.Fatal("the slot did not open when it said")
	}
}
