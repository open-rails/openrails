//go:build integration

package spendgate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/stretchr/testify/require"
)

// clock is a mutable test clock so anchored-window resets can be exercised
// deterministically (advance time → a fixed window rolls to a new bucket) without
// real sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newGate(t *testing.T) (*spendgate.Gate, *clock, context.Context) {
	t.Helper()
	rdb, ctx := dbtest.SharedRedisClient(t)

	g := spendgate.New(rdb)
	clk := &clock{t: time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)}
	g.SetClock(clk.now)
	return g, clk, ctx
}

// reqIDs are unique per call so each admit is a distinct request.
func rid() string { return uuid.NewString() }

func payer() (string, string, string) { return uuid.NewString(), uuid.NewString(), "USD" }

func TestGate_AffordabilityGate_Prepaid(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()

	// accountBalance 1000, prepaid (creditLimit 0): first 600 fits, second 600 does not
	// (1000 - 600 held - 600 < 0).
	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: 1000})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: 1000})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.True(t, d.BlockedBalance)

	held, err := g.HeldAmount(ctx, m, c, cur)
	require.NoError(t, err)
	require.Equal(t, int64(600), held)
}

func TestGate_CreditLimit_Arrears(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()

	// balance 0, credit line 500: floor = -500. 400 fits (0-0-400 >= -500); a further
	// 200 does not (0-400-200 = -600 < -500).
	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 400, AccountBalance: 0, CreditLimit: 500})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 200, AccountBalance: 0, CreditLimit: 500})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.True(t, d.BlockedBalance)
}

func fixedPayerPolicy(limit int64, dur time.Duration) spendgate.Policy {
	return spendgate.Policy{Scopes: []spendgate.ScopedWindows{{
		Scope: spendgate.ScopePayer,
		Windows: []spendgate.Window{{
			Scope: spendgate.ScopePayer, Duration: dur, Limit: limit, Key: "win",
		}},
	}}}
}

func TestGate_WindowGate_FixedResets(t *testing.T) {
	g, clk, ctx := newGate(t)
	m, c, cur := payer()
	pol := fixedPayerPolicy(1000, 5*time.Hour)
	big := int64(1_000_000) // balance never binds

	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Second 600 would breach the 1000 window (600+600>1000).
	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.NotNil(t, d.BlockedWindow)
	require.Equal(t, "win", d.BlockedWindow.Key)

	// Advance past the fixed window: a new bucket resets the counter.
	clk.add(5*time.Hour + time.Second)
	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.True(t, d.Allowed, "window should reset in the next fixed bucket")
}

func TestGate_Idempotent(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	r := rid()

	for i := 0; i < 3; i++ {
		d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: r, Cost: 500, AccountBalance: 1000})
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	held, err := g.HeldAmount(ctx, m, c, cur)
	require.NoError(t, err)
	require.Equal(t, int64(500), held, "replaying the same request must not double-reserve")
}

func TestGate_CaptureFreesHeldKeepsWindow(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	pol := fixedPayerPolicy(1000, 5*time.Hour)
	big := int64(1_000_000)
	r := rid()

	_, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: r, Cost: 500, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.NoError(t, g.Capture(ctx, spendgate.CaptureInput{Merchant: m, Customer: c, Currency: cur, RequestID: r}))

	held, err := g.HeldAmount(ctx, m, c, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), held, "capture frees the held reservation")

	// The window kept the estimate (estimate-based): 500 already counted, so a 600
	// request now breaches the 1000 cap.
	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 600, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.NotNil(t, d.BlockedWindow)
}

func TestGate_ReleaseFreesHeldAndWindow(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	pol := fixedPayerPolicy(1000, 5*time.Hour)
	big := int64(1_000_000)
	r := rid()

	_, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: r, Cost: 500, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.NoError(t, g.Release(ctx, spendgate.ReleaseInput{Merchant: m, Customer: c, Currency: cur, RequestID: r}))

	held, err := g.HeldAmount(ctx, m, c, cur)
	require.NoError(t, err)
	require.Equal(t, int64(0), held, "release frees the held reservation")

	// The window was freed too (the request did not happen): a full 1000 fits again.
	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 1000, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.True(t, d.Allowed, "release should free the window reservation")
}

func TestGate_MultiScopeDenyOnAnyWindow(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	big := int64(1_000_000)
	// payer window allows 1000; invoker window allows only 300.
	pol := spendgate.Policy{Scopes: []spendgate.ScopedWindows{
		{Scope: spendgate.ScopePayer, Windows: []spendgate.Window{{Scope: spendgate.ScopePayer, Duration: time.Hour, Limit: 1000, Key: "p"}}},
		{Scope: spendgate.ScopeInvoker, ScopeID: "svc:abc", Windows: []spendgate.Window{{Scope: spendgate.ScopeInvoker, Duration: time.Hour, Limit: 300, Key: "i"}}},
	}}
	req := spendgate.Request{Invoker: "svc:abc"}

	// 500 fits the payer window but breaches the invoker window → denied on invoker.
	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 500, AccountBalance: big, Policy: pol, Request: req})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.NotNil(t, d.BlockedWindow)
	require.Equal(t, "i", d.BlockedWindow.Key)

	// A different invoker is unconstrained by the svc:abc window → 500 fits the payer cap.
	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 500, AccountBalance: big, Policy: pol, Request: spendgate.Request{Invoker: "svc:other"}})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

func TestGate_RoleWindowIsPerInvoker(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	big := int64(1_000_000)
	pol := spendgate.Policy{Scopes: []spendgate.ScopedWindows{{
		Scope:   spendgate.ScopeRole,
		ScopeID: "role:editors",
		Windows: []spendgate.Window{{
			Scope: spendgate.ScopeRole, Duration: time.Hour, Limit: 1000, Key: "role-1h",
		}},
	}}}
	admit := func(invoker string, cost int64) spendgate.Decision {
		t.Helper()
		d, err := g.Admit(ctx, spendgate.AdmitInput{
			Merchant:       m,
			Customer:       c,
			Currency:       cur,
			RequestID:      rid(),
			Cost:           cost,
			AccountBalance: big,
			Policy:         pol,
			Request:        spendgate.Request{Invoker: invoker, Roles: []string{"role:editors"}},
		})
		require.NoError(t, err)
		return d
	}

	require.True(t, admit("user:one", 700).Allowed)
	d := admit("user:one", 400)
	require.False(t, d.Allowed, "same invoker should exhaust its own role window")
	require.NotNil(t, d.BlockedWindow)
	require.Equal(t, "role-1h", d.BlockedWindow.Key)

	require.True(t, admit("user:two", 700).Allowed, "different invoker with same role should get an independent role window")
}

func TestGate_DelegatedScopesPerInvokerPayerScopeAggregate(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	big := int64(1_000_000)

	t.Run("invoker scope is per concrete invoker", func(t *testing.T) {
		pol := spendgate.Policy{Scopes: []spendgate.ScopedWindows{
			{Scope: spendgate.ScopeInvoker, ScopeID: "user:one", Windows: []spendgate.Window{{Scope: spendgate.ScopeInvoker, Duration: time.Hour, Limit: 1000, Key: "invoker-1h"}}},
			{Scope: spendgate.ScopeInvoker, ScopeID: "user:two", Windows: []spendgate.Window{{Scope: spendgate.ScopeInvoker, Duration: time.Hour, Limit: 1000, Key: "invoker-1h"}}},
		}}
		admit := func(invoker string, cost int64) spendgate.Decision {
			t.Helper()
			d, err := g.Admit(ctx, spendgate.AdmitInput{
				Merchant:       m,
				Customer:       c,
				Currency:       cur,
				RequestID:      rid(),
				Cost:           cost,
				AccountBalance: big,
				Policy:         pol,
				Request:        spendgate.Request{Invoker: invoker},
			})
			require.NoError(t, err)
			return d
		}
		require.True(t, admit("user:one", 700).Allowed)
		require.False(t, admit("user:one", 400).Allowed)
		require.True(t, admit("user:two", 700).Allowed)
	})

	t.Run("payer scope is aggregate", func(t *testing.T) {
		m, c, cur := payer()
		pol := fixedPayerPolicy(1000, time.Hour)
		admit := func(invoker string, cost int64) spendgate.Decision {
			t.Helper()
			d, err := g.Admit(ctx, spendgate.AdmitInput{
				Merchant:       m,
				Customer:       c,
				Currency:       cur,
				RequestID:      rid(),
				Cost:           cost,
				AccountBalance: big,
				Policy:         pol,
				Request:        spendgate.Request{Invoker: invoker},
			})
			require.NoError(t, err)
			return d
		}
		require.True(t, admit("user:one", 700).Allowed)
		d := admit("user:two", 400)
		require.False(t, d.Allowed, "payer scope should stay aggregate across invokers")
		require.NotNil(t, d.BlockedWindow)
		require.Equal(t, "win", d.BlockedWindow.Key)
	})
}

func TestGate_ResolvePointer(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	r := rid()

	_, ok, err := g.Resolve(ctx, m, r)
	require.NoError(t, err)
	require.False(t, ok, "no pointer before admit")

	_, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: r, Invoker: "user:x", Source: "usage", Cost: 100, AccountBalance: 1000})
	require.NoError(t, err)

	ref, ok, err := g.Resolve(ctx, m, r)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, c, ref.Customer)
	require.Equal(t, cur, ref.Currency)
	require.Equal(t, "user:x", ref.Invoker)
	require.Equal(t, "usage", ref.Source)

	// Capture removes the pointer.
	require.NoError(t, g.Capture(ctx, spendgate.CaptureInput{Merchant: m, Customer: c, Currency: cur, RequestID: r}))
	_, ok, err = g.Resolve(ctx, m, r)
	require.NoError(t, err)
	require.False(t, ok, "pointer cleared after capture")
}

func TestGate_WindowAccumulates(t *testing.T) {
	g, _, ctx := newGate(t)
	m, c, cur := payer()
	big := int64(1_000_000)
	pol := spendgate.Policy{Scopes: []spendgate.ScopedWindows{{
		Scope:   spendgate.ScopePayer,
		Windows: []spendgate.Window{{Scope: spendgate.ScopePayer, Duration: time.Hour, Limit: 1000, Key: "s"}},
	}}}

	d, err := g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 700, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	d, err = g.Admit(ctx, spendgate.AdmitInput{Merchant: m, Customer: c, Currency: cur, RequestID: rid(), Cost: 400, AccountBalance: big, Policy: pol})
	require.NoError(t, err)
	require.False(t, d.Allowed, "700+400 breaches the 1000 window")
	require.NotNil(t, d.BlockedWindow)
}
