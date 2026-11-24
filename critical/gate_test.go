//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/stretchr/testify/require"
)

// gatePlan counts work units toward an hourly and a daily cap; usage is
// money at a host-stated price.
const gatePlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  hourly:
    unit: units
    reset: {kind: fixed, window_seconds: 3600, anchor: first_use}
    counts: [{meter: work}]
  daily:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {hourly: "100", daily: "60"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// sessionPlan is tokenPlan with a session of 8 seconds, which ends within
// one lease's life.
const sessionPlan = `schema_version: 1
meters:
  input-tokens: {event_type: chat, value_property: input_tokens, aggregation: sum, group_by: {model: model}}
limits:
  session:
    unit: usage_units
    reset: {kind: session, window_seconds: 8}
    overrun: admit_while_positive
    counts:
    - {meter: input-tokens, per: 1000, filter: {model: [small]}}
    - {meter: input-tokens, weight: 5, per: 1000, filter: {model: [large]}}
  weekly:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 604800, anchor: first_use}
    overrun: admit_while_positive
    counts:
    - {meter: input-tokens, per: 1000, filter: {model: [small]}}
    - {meter: input-tokens, weight: 5, per: 1000, filter: {model: [large]}}
products:
  pro: {display_name: Pro, limits: {session: "1000", weekly: "1500"}}
  chat: {display_name: Chat, rate_cards: [{meter: input-tokens, price: {model: dynamic, currency: USD}}]}
`

// tickPlan counts work units toward ten-second windows tiled from the epoch.
const tickPlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
limits:
  tick:
    unit: units
    reset: {kind: fixed, window_seconds: 10, anchor: subscription}
    counts: [{meter: work}]
products:
  plan: {display_name: Plan, limits: {tick: "5"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// gateWire is a remote gate's transport to OpenRails: it counts lease
// syncs, can be cut, can lose a sync's reply after the server answered, and
// can watch each sync (watch answers whether to lose that sync's reply).
type gateWire struct {
	http.RoundTripper
	syncs     atomic.Int64
	down      atomic.Bool
	dropReply atomic.Int64
	watch     func(billing.SyncLeasesParams, *billing.SyncLeasesResult) bool
}

func (w *gateWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if w.down.Load() {
		return nil, errors.New("critical fixture cannot reach OpenRails")
	}
	if r.URL.Path != "/v1/app/leases/sync" {
		return w.RoundTripper.RoundTrip(r)
	}
	w.syncs.Add(1)
	var params billing.SyncLeasesParams
	if w.watch != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := json.Unmarshal(body, &params); err != nil {
			return nil, err
		}
	}
	resp, err := w.RoundTripper.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	lose := false
	if w.watch != nil && resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var out billing.SyncLeasesResult
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		lose = w.watch(params, &out)
	}
	if lose || w.dropReply.Load() > 0 {
		if !lose {
			w.dropReply.Add(-1)
		}
		_ = resp.Body.Close()
		return nil, errors.New("critical fixture lost the sync's reply")
	}
	return resp, err
}

func gateRemote(t *testing.T, a *authorityWorld) (*openrails.Client, *gateWire) {
	t.Helper()
	wire := &gateWire{RoundTripper: http.DefaultTransport}
	remote, err := openrails.NewRemote(a.server.URL, openrails.RemoteOptions{
		HTTPClient:  &http.Client{Transport: wire, Timeout: 10 * time.Second},
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{a.alpha.MerchantID(): {APIKey: a.alphaKey.Secret}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close(context.Background())) })
	client, err := remote.Merchant(a.alpha.MerchantID())
	require.NoError(t, err)
	return client, wire
}

func newGate(t *testing.T, client *openrails.Client, cfg openrails.GateDeps) *openrails.Gate {
	t.Helper()
	g, err := openrails.NewGate(client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = g.Close(ctx)
	})
	return g
}

func gateWork(customer billing.CustomerID, id string, units int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": units}, Amount: ptr(int64(0))}}
}

func workUsage(units int64) billing.CaptureUsage {
	return billing.CaptureUsage{Dimensions: map[string]int64{"units": units}}
}

func gateAdmit(t *testing.T, g *openrails.Gate, p billing.AdmitParams) *openrails.Ticket {
	t.Helper()
	ticket, err := g.Admit(t.Context(), p)
	require.NoError(t, err)
	require.True(t, ticket.Allowed(), "%s refused: %+v", p.RequestID, ticket.Refusal())
	return ticket
}

func gateFlush(t *testing.T, g *openrails.Gate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	require.NoError(t, g.Flush(ctx))
}

func gateCustomer(t *testing.T, a *authorityWorld) billing.CustomerID {
	t.Helper()
	customer := billing.CustomerID(uuid.New())
	grantLimitProduct(t, a.alpha, customer, "plan", 48)
	return customer
}

// memOutbox is the host's durable store, as a map that outlives its gates.
type memOutbox struct {
	mu    sync.Mutex
	items map[billing.LeaseUsageRef]billing.LeaseUsage
}

func (o *memOutbox) Put(_ context.Context, items []billing.OutboxItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range items {
		o.items[billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}] = it.Usage
	}
	return nil
}

func (o *memOutbox) Pending(context.Context) ([]billing.OutboxItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]billing.OutboxItem, 0, len(o.items))
	for ref, u := range o.items {
		out = append(out, billing.OutboxItem{LeaseID: ref.LeaseID, Usage: u})
	}
	return out, nil
}

func (o *memOutbox) Ack(_ context.Context, refs []billing.LeaseUsageRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range refs {
		delete(o.items, r)
	}
	return nil
}

func (o *memOutbox) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.items)
}

// computeWork is one compute request of units (usageMeter).
func computeWork(customer billing.CustomerID, id string, units int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "compute", Dimensions: map[string]int64{"units": units}}}
}

func TestGate(t *testing.T) {
	t.Run("OR-J23-S19/remote-and-embedded-gates-decide-warm-without-openrails", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		remoteClient, wire := gateRemote(t, a)
		for name, client := range map[string]*openrails.Client{"embedded": a.alpha, "remote": remoteClient} {
			customer := gateCustomer(t, a)
			g := newGate(t, client, openrails.GateDeps{})
			gateAdmit(t, g, gateWork(customer, name+"-cold", 2)).Release(t.Context())
			for i := range 5 {
				_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("%s-%d", name, i), 1))
				require.NoError(t, err)
			}
			gateFlush(t, g)
			require.EqualValues(t, 5, customerLimit(t, a.alpha, customer, "daily").Used, "%s: each consumed unit settles once", name)
		}

		customer := gateCustomer(t, a)
		g := newGate(t, remoteClient, openrails.GateDeps{})
		gateAdmit(t, g, gateWork(customer, "warm-up", 1)).Release(t.Context())
		wire.down.Store(true)
		const warm = 200
		started := time.Now()
		for i := range warm {
			ticket, err := g.Admit(t.Context(), gateWork(customer, fmt.Sprintf("warm-%d", i), 1))
			require.NoError(t, err, "a warm decision needs no I/O")
			require.True(t, ticket.Allowed())
			require.NoError(t, ticket.Release(t.Context()))
		}
		t.Logf("%d warm decisions with OpenRails unreachable, mean %s", warm, time.Since(started)/warm)
		// Once its sync fails the gate decides on its view within its share.
		time.Sleep(1500 * time.Millisecond)
		ticket, err := g.Admit(t.Context(), gateWork(customer, "offline", 1))
		require.NoError(t, err)
		require.True(t, ticket.Allowed(), "a view keeps deciding while OpenRails is unreachable")
		_, err = g.Admit(t.Context(), gateWork(gateCustomer(t, a), "cold", 1))
		require.ErrorIs(t, err, openrails.ErrUnavailable, "a cold customer needs OpenRails")
		wire.down.Store(false)
	})

	t.Run("OR-J23-S20/racing-requests-on-two-limits-never-take-more-than-either-limit", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateDeps{})
		var mu sync.Mutex
		admitted := int64(0)
		var wg sync.WaitGroup
		for w := range 12 {
			wg.Go(func() {
				for i := range 8 {
					units := int64(1 + (w+i)%3)
					ticket, err := g.Admit(t.Context(), gateWork(customer, fmt.Sprintf("race-%d-%d", w, i), units))
					if err != nil {
						t.Errorf("admit: %v", err)
						return
					}
					if !ticket.Allowed() {
						if r := ticket.Refusal(); r.DenyCode != billing.DenyLimitReached || r.Limit == nil || *r.Limit != "daily" {
							t.Errorf("refusal %+v, want daily limit_reached", r)
						}
						continue
					}
					mu.Lock()
					admitted += units
					mu.Unlock()
					if err := ticket.Capture(t.Context(), workUsage(units)); err != nil {
						t.Errorf("capture: %v", err)
					}
				}
			})
		}
		wg.Wait()
		gateFlush(t, g)
		daily, hourly := customerLimit(t, a.alpha, customer, "daily"), customerLimit(t, a.alpha, customer, "hourly")
		require.Equal(t, admitted, daily.Used)
		require.Equal(t, admitted, hourly.Used)
		require.LessOrEqual(t, daily.Used, daily.Capacity, "one gate never admits past the daily cap")

		// Eighty of a hundred taken and thirty captured leaves seventy.
		other := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, other, "plan", 48)
		_, err := a.alpha.UpdateCustomer(t.Context(), other, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "daily", Capacity: 100}}})
		require.NoError(t, err)
		eighty := gateAdmit(t, g, gateWork(other, "eighty", 80))
		require.NoError(t, eighty.Capture(t.Context(), workUsage(30)))
		gateFlush(t, g)
		gateAdmit(t, g, gateWork(other, "seventy", 70))
		refused, err := g.Admit(t.Context(), gateWork(other, "one-more", 1))
		require.NoError(t, err)
		require.False(t, refused.Allowed())
		require.Equal(t, billing.DenyLimitReached, refused.Refusal().DenyCode)
		gateFlush(t, g)
		require.EqualValues(t, 30, customerLimit(t, a.alpha, other, "daily").Used)
	})

	t.Run("OR-J23-S21/restarts-and-lost-replies-converge-once", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)

		// A lost reply is resent under the same numbers and settles once.
		client, wire := gateRemote(t, a)
		g := newGate(t, client, openrails.GateDeps{})
		for i := range 4 {
			_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("lost-%d", i), 2))
			require.NoError(t, err)
		}
		wire.dropReply.Store(1)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		require.ErrorIs(t, g.Flush(ctx), openrails.ErrUnavailable)
		gateFlush(t, g)
		require.EqualValues(t, 8, customerLimit(t, a.alpha, customer, "daily").Used)

		// A process stops with unsent usage: with an outbox a new process
		// resends it against the old lease; without one it is never billed.
		outbox := &memOutbox{items: map[billing.LeaseUsageRef]billing.LeaseUsage{}}
		durableClient, durableWire := gateRemote(t, a)
		durable := newGate(t, durableClient, openrails.GateDeps{Outbox: outbox})
		plainClient, plainWire := gateRemote(t, a)
		plain := newGate(t, plainClient, openrails.GateDeps{})
		gateAdmit(t, durable, gateWork(customer, "durable-warm", 3)).Release(t.Context())
		gateAdmit(t, plain, gateWork(customer, "plain-warm", 5)).Release(t.Context())
		gateFlush(t, durable)
		gateFlush(t, plain)
		durableWire.down.Store(true)
		plainWire.down.Store(true)
		_, err := durable.Consume(t.Context(), gateWork(customer, "unsent-durable", 3))
		require.NoError(t, err)
		_, err = plain.Consume(t.Context(), gateWork(customer, "unsent-plain", 5))
		require.NoError(t, err)
		for _, stopped := range []*openrails.Gate{durable, plain} {
			stop, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			require.ErrorIs(t, stopped.Close(stop), openrails.ErrUnavailable, "the stopping process cannot send its usage")
			cancel()
		}
		require.Equal(t, 1, outbox.len())
		restartedClient, _ := gateRemote(t, a)
		restarted := newGate(t, restartedClient, openrails.GateDeps{Outbox: outbox})
		require.Eventually(t, func() bool { return outbox.len() == 0 }, 10*time.Second, 50*time.Millisecond, "the restarted process resends and acknowledges")
		require.EqualValues(t, 11, customerLimit(t, a.alpha, customer, "daily").Used, "the outbox's 3 units settle once; the 5 without one never")

		// The stopped processes' leases lapse: nothing they had counts.
		a.clock.Advance(limits.LeaseLifetime + time.Second)
		_, err = restarted.Consume(t.Context(), gateWork(customer, "after", 1))
		require.NoError(t, err)
		gateFlush(t, restarted)
		require.EqualValues(t, 12, customerLimit(t, a.alpha, customer, "daily").Used)
		require.NoError(t, restarted.Close(t.Context()))
		require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "no stopped lease holds anything")
	})

	t.Run("OR-J23-S22/a-ticket-lives-across-syncs-and-captures-across-a-session-reset", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, sessionPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, customer, "pro", 24*30)
		// The gate reads windows by its own clock: the world's starts at it.
		a.clock.Advance(time.Since(a.clock.Now()))
		g := newGate(t, a.alpha, openrails.GateDeps{})
		chat := func(id string, tokens int64) billing.AdmitParams {
			return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
				Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"input_tokens": tokens}, Metadata: map[string]any{"model": "large"}, Amount: ptr(int64(0))}}
		}
		capture := func(tokens int64) billing.CaptureUsage {
			return billing.CaptureUsage{Dimensions: map[string]int64{"input_tokens": tokens}, Metadata: map[string]any{"model": "large"}}
		}

		long := gateAdmit(t, g, chat("long", 20_000))
		time.Sleep(2500 * time.Millisecond)
		require.NoError(t, long.Capture(t.Context(), capture(40_000)), "a ticket outlives many syncs with no deadline")
		gateFlush(t, g)
		require.EqualValues(t, 200, customerLimit(t, a.alpha, customer, "session").Used)

		// The session (8 s) ends before the capture settles.
		late := gateAdmit(t, g, chat("late", 20_000))
		a.clock.Advance(9 * time.Second)
		require.NoError(t, late.Capture(t.Context(), capture(20_000)))
		gateFlush(t, g)
		require.Nil(t, customerLimit(t, a.alpha, customer, "session").WindowStartsAt, "the capture settles in its ended session and opens none")
		require.EqualValues(t, 300, customerLimit(t, a.alpha, customer, "weekly").Used)
	})

	t.Run("OR-J23-S24/refusals-are-verdicts-and-openrails-down-is-unavailable", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateDeps{})
		_, err := g.Consume(t.Context(), gateWork(customer, "most", 55))
		require.NoError(t, err)
		refused, err := g.Admit(t.Context(), gateWork(customer, "past", 10))
		require.NoError(t, err)
		r := refused.Refusal()
		require.NotNil(t, r)
		require.Equal(t, billing.DenyLimitReached, r.DenyCode)
		require.Equal(t, "daily", *r.Limit)
		require.NotNil(t, r.ResetsAt, "an exhausted limit says when it resets")
		// Near the limit the server decides what still fits.
		gateAdmit(t, g, gateWork(customer, "last", 5))
		admission, err := a.alpha.GetAdmission(t.Context(), "last")
		require.NoError(t, err)
		require.Nil(t, admission.ExpiresAt, "a gate's request hold lives with its lease")

		// With OpenRails down a cold customer is ErrUnavailable.
		client, wire := gateRemote(t, a)
		down := newGate(t, client, openrails.GateDeps{})
		wire.down.Store(true)
		_, err = down.Admit(t.Context(), gateWork(gateCustomer(t, a), "down", 1))
		require.ErrorIs(t, err, openrails.ErrUnavailable)
		require.ErrorIs(t, err, billing.ErrUnreachable)
		wire.down.Store(false)
	})

	t.Run("OR-J23-S27/closing-a-gate-ends-its-lease-and-releases-what-it-had", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateDeps{})
		_, err := g.Consume(t.Context(), gateWork(customer, "one", 2))
		require.NoError(t, err)
		gateAdmit(t, g, gateWork(customer, "open", 3))
		gateAdmit(t, g, gateWork(customer, "near", 50))
		// The open ticket is reported once a sync has passed it.
		time.Sleep(2500 * time.Millisecond)
		require.EqualValues(t, 53, customerLimit(t, a.alpha, customer, "daily").Held, "the gate's hold and its reported ticket")
		require.NoError(t, g.Close(t.Context()))
		daily := customerLimit(t, a.alpha, customer, "daily")
		require.EqualValues(t, 2, daily.Used)
		require.Zero(t, daily.Held, "a closed lease holds nothing, before it would have lapsed")
	})

	t.Run("OR-J23-S37/four-gates-racing-a-small-balance-overshoot-at-most-the-bound", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, a.alpha, customer, "funded", 40_000)
		gates := make([]*openrails.Gate, 4)
		for i := range gates {
			gates[i] = newGate(t, a.alpha, openrails.GateDeps{})
		}
		// Steady traffic, ten requests a second on each gate, until the
		// balance runs out: what others add is learned from the views, and
		// the last of the balance is decided on the server.
		var admitted atomic.Int64
		var wg sync.WaitGroup
		for i, g := range gates {
			wg.Go(func() {
				refusals := 0
				for n := 0; n < 200 && refusals < 3; n++ {
					time.Sleep(100 * time.Millisecond)
					ticket, err := g.Consume(t.Context(), computeWork(customer, fmt.Sprintf("race-%d-%d", i, n), 1))
					if err != nil {
						t.Errorf("consume: %v", err)
						return
					}
					if !ticket.Allowed() {
						refusals++
						continue
					}
					admitted.Add(100)
				}
			})
		}
		wg.Wait()
		for _, g := range gates {
			gateFlush(t, g)
		}
		b, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.Equal(t, admitted.Load(), 40_000-b.AvailableAmount, "every admitted request settled once")
		require.GreaterOrEqual(t, b.AvailableAmount, int64(-4*100), "steady traffic overshoots at most one request a gate")
	})

	t.Run("OR-J23-S38/two-gates-and-an-unreachable-openrails-spend-at-most-the-remaining", func(t *testing.T) {
		a := newAuthorityWorld(t)
		usageMeter(t, a.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, a.alpha, customer, "funded", 10_000)
		type process struct {
			gate *openrails.Gate
			wire *gateWire
		}
		var processes []process
		for range 2 {
			client, wire := gateRemote(t, a)
			processes = append(processes, process{newGate(t, client, openrails.GateDeps{}), wire})
		}
		// Each gate syncs while the other is live, as active gates do.
		for round := range 2 {
			for i, p := range processes {
				_, err := p.gate.Consume(t.Context(), computeWork(customer, fmt.Sprintf("warm-%d-%d", round, i), 1))
				require.NoError(t, err)
				gateFlush(t, p.gate)
			}
		}
		for _, p := range processes {
			p.wire.down.Store(true)
		}
		// Each learns at its next sync that OpenRails is unreachable.
		for i, p := range processes {
			_, err := p.gate.Consume(t.Context(), computeWork(customer, fmt.Sprintf("noticed-%d", i), 1))
			require.NoError(t, err)
		}
		time.Sleep(1500 * time.Millisecond)
		offline := int64(0)
		for i, p := range processes {
			for n := range 200 {
				ticket, err := p.gate.Consume(t.Context(), computeWork(customer, fmt.Sprintf("offline-%d-%d", i, n), 1))
				if errors.Is(err, openrails.ErrUnavailable) {
					break
				}
				require.NoError(t, err)
				require.True(t, ticket.Allowed())
				offline++
			}
		}
		require.Positive(t, offline, "a view keeps deciding while OpenRails is unreachable")
		for _, p := range processes {
			p.wire.down.Store(false)
			gateFlush(t, p.gate)
		}
		b, err := a.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.GreaterOrEqual(t, b.AvailableAmount, int64(0), "the two gates together spent at most what was left, each its share")
	})

	t.Run("OR-J23-S39/near-zero-is-decided-on-the-server-exactly", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, unitPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, customer, "plan", 24)
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "quota", Capacity: 10}}})
		require.NoError(t, err)
		settleUsage(t, a.alpha, newLease(), numbered(1, workReport(customer, "most", 9))...)
		gates := []*openrails.Gate{newGate(t, a.alpha, openrails.GateDeps{}), newGate(t, a.alpha, openrails.GateDeps{})}
		for i, g := range gates {
			gateAdmit(t, g, gateWork(customer, fmt.Sprintf("view-%d", i), 0)).Release(t.Context())
		}
		// One unit is left and both gates see it: every request is near zero
		// and decided on the server, one at a time.
		var allowed atomic.Int64
		var wg sync.WaitGroup
		for i, g := range gates {
			for n := range 3 {
				wg.Go(func() {
					ticket, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("last-%d-%d", i, n), 1))
					if err != nil {
						t.Errorf("consume: %v", err)
						return
					}
					if ticket.Allowed() {
						allowed.Add(1)
					}
				})
			}
		}
		wg.Wait()
		for _, g := range gates {
			gateFlush(t, g)
		}
		require.EqualValues(t, 1, allowed.Load(), "exactly the one unit left")
		require.EqualValues(t, 10, customerLimit(t, a.alpha, customer, "quota").Used)
	})

	t.Run("OR-J23-S40/an-idle-gate-stops-syncing-and-decides-one-request-on-its-stale-view", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		client, wire := gateRemote(t, a)
		g := newGate(t, client, openrails.GateDeps{})
		_, err := g.Consume(t.Context(), gateWork(customer, "one", 1))
		require.NoError(t, err)
		gateFlush(t, g)
		time.Sleep(2500 * time.Millisecond)
		quiet := wire.syncs.Load()
		time.Sleep(3 * time.Second)
		require.Equal(t, quiet, wire.syncs.Load(), "an idle gate stops syncing")
		// Its lease lapses: another lease's sync no longer counts it.
		a.clock.Advance(limits.LeaseLifetime + time.Second)
		out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease()})
		require.Equal(t, 1, out.Gates)
		// Its stale view decides one request with no sync, and the next waits
		// for a sync to answer it.
		gateAdmit(t, g, gateWork(customer, "probe", 1)).Release(t.Context())
		require.Equal(t, quiet, wire.syncs.Load(), "the probe was decided on the stale view")
		gateAdmit(t, g, gateWork(customer, "next", 1)).Release(t.Context())
		require.Greater(t, wire.syncs.Load(), quiet, "the next request waited for a sync")
	})

	t.Run("OR-J23-S41/a-window-that-resets-between-syncs-is-decided-from-the-next-window", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, tickPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, customer, "plan", 24)
		a.clock.Advance(time.Since(a.clock.Now()))
		client, wire := gateRemote(t, a)
		g := newGate(t, client, openrails.GateDeps{})
		_, err := g.Consume(t.Context(), gateWork(customer, "first", 1))
		require.NoError(t, err)
		gateFlush(t, g)
		ends := customerLimit(t, a.alpha, customer, "tick").ResetsAt
		require.NotNil(t, ends)
		// Fill the window, then decide the first request of the next one
		// with no sync between.
		for i := range 4 {
			_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("fill-%d", i), 1))
			require.NoError(t, err)
		}
		gateFlush(t, g)
		time.Sleep(time.Until(*ends) + 200*time.Millisecond)
		before := wire.syncs.Load()
		next, err := g.Consume(t.Context(), gateWork(customer, "next-window", 1))
		require.NoError(t, err)
		require.True(t, next.Allowed(), "the view's next window has room: %+v", next.Refusal())
		require.Equal(t, before, wire.syncs.Load(), "decided on the view, with no sync")
		a.clock.Advance(time.Since(a.clock.Now()))
		gateFlush(t, g)
		require.EqualValues(t, 1, customerLimit(t, a.alpha, customer, "tick").Used, "it settles in the window it was admitted in")
	})
}
