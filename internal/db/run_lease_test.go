package db

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestHoldRunLease(t *testing.T) {
	const ttl = 400 * time.Millisecond
	hold := func(t *testing.T, claim func(n int32) (bool, error)) (*RunLease, *atomic.Int32, *atomic.Int32) {
		t.Helper()
		var claims, releases atomic.Int32
		lease, err := holdRunLease(t.Context(), slog.Default(), ttl, func(context.Context) (bool, error) {
			return claim(claims.Add(1))
		}, func(context.Context) error {
			releases.Add(1)
			return nil
		})
		if err != nil || lease == nil {
			t.Fatalf("lease not taken: %v", err)
		}
		t.Cleanup(lease.Release)
		return lease, &claims, &releases
	}
	t.Run("renewals-keep-it-past-its-ttl", func(t *testing.T) {
		lease, claims, releases := hold(t, func(int32) (bool, error) { return true, nil })
		time.Sleep(3 * ttl)
		if err := lease.Context().Err(); err != nil {
			t.Fatalf("a renewed lease ended: %v", context.Cause(lease.Context()))
		}
		if n := claims.Load(); n < 4 {
			t.Fatalf("%d claims in three ttls", n)
		}
		lease.Release()
		lease.Release()
		if n := releases.Load(); n != 1 {
			t.Fatalf("released %d times", n)
		}
	})
	t.Run("a-refused-renewal-ends-the-run", func(t *testing.T) {
		lease, _, _ := hold(t, func(n int32) (bool, error) { return n == 1, nil })
		select {
		case <-lease.Context().Done():
		case <-time.After(ttl):
			t.Fatal("the run outlived a refused renewal")
		}
		if cause := context.Cause(lease.Context()); !errors.Is(cause, ErrLeaseLost) {
			t.Fatalf("cause %v", cause)
		}
	})
	t.Run("unconfirmed-renewals-end-the-run-before-it-could-lapse", func(t *testing.T) {
		start := time.Now()
		lease, _, _ := hold(t, func(n int32) (bool, error) {
			if n == 1 {
				return true, nil
			}
			return false, errors.New("database unreachable")
		})
		select {
		case <-lease.Context().Done():
		case <-time.After(2 * ttl):
			t.Fatal("the run outlived its unconfirmed lease")
		}
		// It ends at four fifths of the ttl from the last confirmed claim.
		if elapsed := time.Since(start); elapsed < ttl*3/4 {
			t.Fatalf("ended after %s, while its lease was still confirmed", elapsed)
		}
		if cause := context.Cause(lease.Context()); !errors.Is(cause, ErrLeaseLost) {
			t.Fatalf("cause %v", cause)
		}
	})
	t.Run("a-lease-another-run-holds-is-not-taken", func(t *testing.T) {
		lease, err := holdRunLease(t.Context(), slog.Default(), ttl, func(context.Context) (bool, error) { return false, nil }, func(context.Context) error {
			t.Fatal("released a lease it never held")
			return nil
		})
		if err != nil || lease != nil {
			t.Fatalf("lease %v, err %v", lease, err)
		}
	})
}
