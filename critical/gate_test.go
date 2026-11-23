//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/limits"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/redis/go-redis/v9"
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
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
  daily:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
products:
  plan: {display_name: Plan, limits: {hourly: "100", daily: "60"}}
  metered: {display_name: Metered, rate_cards: [{meter: work, price: {model: dynamic, currency: USD}}]}
`

// safePlan is gatePlan with money priced per call: a dynamic card charges
// its stated amount only for a positive quantity, so a request of calls and
// no units spends money alone.
const safePlan = `schema_version: 1
meters:
  work: {event_type: work, value_property: units, aggregation: sum}
  calls: {event_type: work, value_property: calls, aggregation: sum}
limits:
  hourly:
    unit: units
    reset: {kind: fixed, window_seconds: 3600, anchor: first_use}
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
  daily:
    unit: units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    rate_cards: [{meter: work, price: {model: per_unit, per_unit: {unit_amount: "1"}}}]
products:
  plan: {display_name: Plan, limits: {hourly: "100", daily: "60"}}
  metered: {display_name: Metered, rate_cards: [{meter: calls, price: {model: dynamic, currency: USD}}]}
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
    rate_cards:
    - meter: input-tokens
      price: {model: per_unit, per_unit: {divide_by: 1000, matrix: {dimension: model, cells: {small: {unit_amount: "1"}, large: {unit_amount: "5"}}}}}
  weekly:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 604800, anchor: first_use}
    overrun: admit_while_positive
    rate_cards:
    - meter: input-tokens
      price: {model: per_unit, per_unit: {divide_by: 1000, matrix: {dimension: model, cells: {small: {unit_amount: "1"}, large: {unit_amount: "5"}}}}}
products:
  pro: {display_name: Pro, limits: {session: "1000", weekly: "1500"}}
  chat: {display_name: Chat, rate_cards: [{meter: input-tokens, price: {model: dynamic, currency: USD}}]}
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
		admitted := 0
		for i := range warm {
			ticket, err := g.Admit(t.Context(), gateWork(customer, fmt.Sprintf("warm-%d", i), 1))
			require.NoError(t, err, "a warm decision needs no I/O")
			require.True(t, ticket.Allowed())
			require.NoError(t, ticket.Release(t.Context()))
			admitted++
		}
		t.Logf("%d warm decisions with OpenRails unreachable, mean %s", admitted, time.Since(started)/warm)
		_, err := g.Admit(t.Context(), gateWork(gateCustomer(t, a), "cold", 1))
		require.ErrorIs(t, err, openrails.ErrUnavailable, "a cold customer needs OpenRails")
		wire.down.Store(false)
	})

	t.Run("OR-J23-S20/racing-requests-on-two-limits-never-take-more-than-either-hold", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateDeps{})
		var mu sync.Mutex
		admitted := int64(0)
		var wg sync.WaitGroup
		race, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for w := range 12 {
			wg.Go(func() {
				for i := range 8 {
					units := int64(1 + (w+i)%3)
					// capacity_held is the gate's own syncs racing; it is waited
					// out, and only the limit itself refuses.
					var ticket *openrails.Ticket
					var err error
					for {
						ticket, err = g.Admit(race, gateWork(customer, fmt.Sprintf("race-%d-%d", w, i), units))
						if err != nil || ticket.Allowed() || ticket.Refusal().DenyCode != billing.DenyCapacityHeld {
							break
						}
					}
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
		require.LessOrEqual(t, daily.Used, daily.Capacity, "admitted cost never passes the daily cap")

		// Holding 80 of 100 and capturing 30 leaves 70.
		other := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, other, "plan", 48)
		_, err := a.alpha.UpdateCustomer(t.Context(), other, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "daily", Capacity: 100}}})
		require.NoError(t, err)
		eighty := gateAdmit(t, g, gateWork(other, "eighty", 80))
		require.NoError(t, eighty.Capture(t.Context(), workUsage(30)))
		gateAdmit(t, g, gateWork(other, "seventy", 70))
		refused, err := g.Admit(t.Context(), gateWork(other, "one-more", 1))
		require.NoError(t, err)
		require.False(t, refused.Allowed())
		require.Equal(t, billing.DenyLimitReached, refused.Refusal().DenyCode)
		gateFlush(t, g)
		require.EqualValues(t, 30, customerLimit(t, a.alpha, other, "daily").Used)
	})

	t.Run("OR-J23-S21/restarts-lost-replies-and-dead-leases-converge-once", func(t *testing.T) {
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

		// The stopped processes' leases die and their holds never reopen.
		a.clock.Advance(limits.LeaseLifetime + limits.LeaseGrace + time.Second)
		gateFlush(t, restarted)
		_, err = restarted.Consume(t.Context(), gateWork(customer, "after", 1))
		require.NoError(t, err)
		gateFlush(t, restarted)
		daily := customerLimit(t, a.alpha, customer, "daily")
		require.EqualValues(t, 12, daily.Used)
		require.NoError(t, restarted.Close(t.Context()))
		a.clock.Advance(limits.LeaseLifetime + limits.LeaseGrace + time.Second)
		require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "no dead lease holds capacity")
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

		// The session ends (8 s) while the lease lives (10 s).
		late := gateAdmit(t, g, chat("late", 20_000))
		a.clock.Advance(9 * time.Second)
		require.NoError(t, late.Capture(t.Context(), capture(20_000)))
		gateFlush(t, g)
		require.Nil(t, customerLimit(t, a.alpha, customer, "session").WindowStartsAt, "the capture settles in its ended session and opens none")
		require.EqualValues(t, 300, customerLimit(t, a.alpha, customer, "weekly").Used)
	})

	t.Run("OR-J23-S23/processes-sharing-redis-form-one-gate", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		type process struct {
			gate  *openrails.Gate
			wire  *gateWire
			redis redis.UniversalClient
		}
		start := func() process {
			client, wire := gateRemote(t, a)
			r := criticalRedis(t)
			return process{gate: newGate(t, client, openrails.GateDeps{Redis: r}), wire: wire, redis: r}
		}
		first := start()
		gateAdmit(t, first.gate, gateWork(customer, "first", 2)).Release(t.Context())
		others := []process{start(), start()}
		time.Sleep(1500 * time.Millisecond)
		for i, p := range others {
			_, err := p.gate.Consume(t.Context(), gateWork(customer, fmt.Sprintf("peer-%d", i), 1))
			require.NoError(t, err, "a customer one process served is warm on the others")
		}
		require.Zero(t, others[0].wire.syncs.Load()+others[1].wire.syncs.Load(), "one process syncs the group")

		// Racing processes never take more than the group's hold.
		gateFlush(t, first.gate)
		for _, p := range append(others, first) {
			p.wire.down.Store(true)
		}
		time.Sleep(1500 * time.Millisecond)
		held := customerLimit(t, a.alpha, customer, "daily").Held
		require.Positive(t, held)
		var taken atomic.Int64
		var wg sync.WaitGroup
		for i, p := range append(others, first) {
			for w := range 4 {
				wg.Go(func() {
					for n := 0; ; n++ {
						ticket, err := p.gate.Admit(t.Context(), gateWork(customer, fmt.Sprintf("race-%d-%d-%d", i, w, n), 1))
						if err != nil || !ticket.Allowed() {
							return
						}
						taken.Add(1)
						if err := ticket.Capture(t.Context(), workUsage(1)); err != nil {
							t.Errorf("capture: %v", err)
							return
						}
					}
				})
			}
		}
		wg.Wait()
		require.LessOrEqual(t, taken.Load(), held, "three processes took %d of a %d hold", taken.Load(), held)
		for _, p := range append(others, first) {
			p.wire.down.Store(false)
		}
		// A cold request answers ErrUnavailable for a sync interval after a
		// sync that could not reach OpenRails: let the group sync again.
		syncs := func() int64 {
			return first.wire.syncs.Load() + others[0].wire.syncs.Load() + others[1].wire.syncs.Load()
		}
		synced := syncs()
		require.Eventually(t, func() bool { return syncs() > synced }, 10*time.Second, 20*time.Millisecond)
		time.Sleep(limits.SyncInterval)

		// Killing the syncing process hands syncing over; no usage is lost or
		// sent twice.
		_, err := first.gate.Consume(t.Context(), gateWork(customer, "before-kill", 1))
		require.NoError(t, err)
		first.wire.down.Store(true)
		require.NoError(t, first.redis.Close())
		for i, p := range others {
			_, err := p.gate.Consume(t.Context(), gateWork(customer, fmt.Sprintf("after-kill-%d", i), 1))
			require.NoError(t, err)
		}
		gateFlush(t, others[0].gate)
		used := 2 + taken.Load() + 1 + 2
		require.Equal(t, used, customerLimit(t, a.alpha, customer, "daily").Used)
		require.Positive(t, others[0].wire.syncs.Load()+others[1].wire.syncs.Load(), "a surviving process took over syncing")

		// With Redis down each process admits from its own lease, and the
		// group lease's capacity comes back when it expires.
		for _, p := range others {
			require.NoError(t, p.redis.Close())
		}
		for i, p := range others {
			_, err := p.gate.Consume(t.Context(), gateWork(customer, fmt.Sprintf("own-lease-%d", i), 1))
			require.NoError(t, err)
			gateFlush(t, p.gate)
		}
		used += 2
		require.Equal(t, used, customerLimit(t, a.alpha, customer, "daily").Used)
		for _, p := range others {
			require.NoError(t, p.gate.Close(t.Context()), "closing returns each process's own holds")
		}
		require.Positive(t, customerLimit(t, a.alpha, customer, "daily").Held, "the group lease still holds until it expires")
		a.clock.Advance(limits.LeaseLifetime + limits.LeaseGrace + time.Second)
		require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "the expired group lease's capacity came back")
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

		// Another process holds what is left: the request waits it out, or is
		// refused capacity_held when its context ends first.
		holder := newGate(t, a.alpha, openrails.GateDeps{})
		contender := newGate(t, a.alpha, openrails.GateDeps{})
		other := gateCustomer(t, a)
		held := gateAdmit(t, holder, gateWork(other, "held", 50))
		short, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		waiting, err := contender.Admit(short, gateWork(other, "short", 20))
		require.NoError(t, err)
		require.False(t, waiting.Allowed())
		require.Equal(t, billing.DenyCapacityHeld, waiting.Refusal().DenyCode)
		require.NoError(t, held.Release(t.Context()))
		patient, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		ticket, err := contender.Admit(patient, gateWork(other, "patient", 20))
		require.NoError(t, err)
		require.True(t, ticket.Allowed(), "the released capacity reaches the waiting process: %+v", ticket.Refusal())

		// With OpenRails down a cold customer is ErrUnavailable.
		client, wire := gateRemote(t, a)
		down := newGate(t, client, openrails.GateDeps{})
		wire.down.Store(true)
		_, err = down.Admit(t.Context(), gateWork(gateCustomer(t, a), "down", 1))
		require.ErrorIs(t, err, openrails.ErrUnavailable)
		require.ErrorIs(t, err, billing.ErrUnreachable)
		wire.down.Store(false)
	})

	t.Run("OR-J23-S25/safe-spend-names-each-holds-money-size-and-never-spends-units", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) {
			alpha := w.cfg.Merchants["alpha"]
			alpha.UsageGateSafeSpend = ptr(true)
			w.cfg.Merchants["alpha"] = alpha
		})
		cfg, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.NotNil(t, cfg.UsageGateSafeSpend)
		require.True(t, *cfg.UsageGateSafeSpend)
		paying := func(client *openrails.Client) billing.CustomerID {
			customer := billing.CustomerID(uuid.New())
			grantLimitProduct(t, client, customer, "plan", 48)
			grantLimitProduct(t, client, customer, "metered", 48)
			fund(t, client, customer, "safe", 1_000_000)
			return customer
		}
		spend := func(customer billing.CustomerID, id string, units, amount int64) billing.AdmitParams {
			p := gateWork(customer, id, units)
			p.Usage.Amount = ptr(amount)
			if amount > 0 {
				p.Usage.Dimensions["calls"] = 1
			}
			return p
		}
		want := func(customer billing.CustomerID) billing.LeaseWant {
			p := spend(customer, "", 0, 100)
			return billing.LeaseWant{CustomerID: customer, Invoker: p.Invoker, InvokerType: p.InvokerType, Currency: "USD", Usage: p.Usage}
		}
		holdOf := func(out *billing.SyncLeasesResult, customer billing.CustomerID, set string) billing.Hold {
			for _, h := range out.Holds {
				if h.CustomerID == customer && h.LimitSet == set {
					return h
				}
			}
			require.Failf(t, "hold not answered", "%+v", out.Holds)
			return billing.Hold{}
		}

		// A merchant that enables it: every hold's safe is its money size.
		applyLimits(t, a.alpha, safePlan)
		direct := paying(a.alpha)
		out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Wants: []billing.LeaseWant{want(direct)}})
		set, refusal := wanted(t, out)
		require.Nil(t, refusal)
		held, _ := heldIn(out, direct, set, "balance")
		require.EqualValues(t, 100, held)
		safe := holdOf(out, direct, set).Safe
		require.NotNil(t, safe)
		require.Equal(t, held, *safe)
		// One that does not: none.
		applyLimits(t, a.beta, safePlan)
		plain := paying(a.beta)
		out = syncLease(t, a.beta, billing.SyncLeasesParams{LeaseID: newLease(), Wants: []billing.LeaseWant{want(plain)}})
		set, refusal = wanted(t, out)
		require.Nil(t, refusal)
		require.Nil(t, holdOf(out, plain, set).Safe)

		// A gate whose hold settled at one request's money spends it, then as
		// much again from safe while OpenRails is unreachable, and no unit
		// past its hold.
		customer := paying(a.alpha)
		client, wire := gateRemote(t, a)
		g := newGate(t, client, openrails.GateDeps{})
		require.NoError(t, gateAdmit(t, g, spend(customer, "warm", 0, 100)).Release(t.Context()))
		require.Eventually(t, func() bool {
			page, err := a.alpha.ListCustomerLimits(t.Context(), customer)
			if err != nil {
				return false
			}
			for _, line := range page.Items {
				if line.Limit == "balance" {
					return line.Held == 100
				}
			}
			return false
		}, 10*time.Second, 100*time.Millisecond, "an idle hold settles at one request")
		wire.down.Store(true)
		_, err = g.Admit(t.Context(), spend(customer, "units", 1, 0))
		require.ErrorIs(t, err, openrails.ErrUnavailable, "safe never covers units")
		gateAdmit(t, g, spend(customer, "from-hold", 0, 100))
		gateAdmit(t, g, spend(customer, "from-safe", 0, 100))
		_, err = g.Admit(t.Context(), spend(customer, "past-safe", 0, 100))
		require.ErrorIs(t, err, openrails.ErrUnavailable)
		wire.down.Store(false)
	})

	t.Run("OR-J23-S27/closing-a-gate-ends-its-lease-and-a-shared-lease-ends-with-its-last-process", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)

		// In process: Close settles the usage and returns the holds at once.
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateDeps{})
		_, err := g.Consume(t.Context(), gateWork(customer, "one", 2))
		require.NoError(t, err)
		require.Positive(t, customerLimit(t, a.alpha, customer, "daily").Held)
		require.NoError(t, g.Close(t.Context()))
		daily := customerLimit(t, a.alpha, customer, "daily")
		require.EqualValues(t, 2, daily.Used)
		require.Zero(t, daily.Held, "a closed lease holds nothing, before it would have expired")

		// Sharing Redis: the group's lease lives while a process remains and
		// ends with the last one to close.
		shared := gateCustomer(t, a)
		start := func() *openrails.Gate {
			client, _ := gateRemote(t, a)
			return newGate(t, client, openrails.GateDeps{Redis: criticalRedis(t)})
		}
		first, second := start(), start()
		_, err = first.Consume(t.Context(), gateWork(shared, "first", 1))
		require.NoError(t, err)
		gateFlush(t, first)
		require.NoError(t, first.Close(t.Context()))
		require.Positive(t, customerLimit(t, a.alpha, shared, "daily").Held, "the remaining process keeps the group's lease")
		_, err = second.Consume(t.Context(), gateWork(shared, "second", 1))
		require.NoError(t, err)
		require.NoError(t, second.Close(t.Context()))
		daily = customerLimit(t, a.alpha, shared, "daily")
		require.EqualValues(t, 2, daily.Used)
		require.Zero(t, daily.Held, "the last process closed the group's lease")
	})

	t.Run("OR-J23-S28/a-units-only-plan-takes-zero-money-through-redis-and-gives-every-unit-back", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		client, _ := gateRemote(t, a)
		g := newGate(t, client, openrails.GateDeps{Redis: criticalRedis(t)})
		// Every hold has a balance part; on a plan with no money it takes 0,
		// which each script takes, releases, captures and settles.
		for i := range 30 {
			require.NoError(t, gateAdmit(t, g, gateWork(customer, fmt.Sprintf("cycle-%d", i), 2)).Release(t.Context()))
		}
		for i := range 10 {
			_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("used-%d", i), 2))
			require.NoError(t, err)
		}
		gateFlush(t, g)
		require.EqualValues(t, 20, customerLimit(t, a.alpha, customer, "daily").Used)
		// Every released unit came back: the 40 left of 60 are admitted.
		for i := range 4 {
			_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("rest-%d", i), 10))
			require.NoError(t, err)
		}
		gateFlush(t, g)
		require.EqualValues(t, 60, customerLimit(t, a.alpha, customer, "daily").Used)
		refused, err := g.Admit(t.Context(), gateWork(customer, "past", 1))
		require.NoError(t, err)
		require.False(t, refused.Allowed())
		require.Equal(t, billing.DenyLimitReached, refused.Refusal().DenyCode)
	})

	t.Run("OR-J23-S29/a-lost-reply-after-a-shrink-keeps-the-capacity-until-the-gate-applies-it", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		client, wire := gateRemote(t, a)
		var mu sync.Mutex
		var version int64
		var lost, checked bool
		var afterLoss []billing.HoldApplied
		var heldAfterLoss int64
		var readErr error
		wire.watch = func(p billing.SyncLeasesParams, out *billing.SyncLeasesResult) bool {
			mu.Lock()
			defer mu.Unlock()
			if lost && !checked {
				// The first sync after the lost reply: the gate has not applied
				// the shrink, so the server keeps what it held.
				checked, afterLoss = true, p.Applied
				page, err := a.alpha.ListCustomerLimits(context.Background(), customer)
				readErr = err
				if err == nil {
					for _, line := range page.Items {
						if line.Limit == "daily" {
							heldAfterLoss = line.Held
						}
					}
				}
				return false
			}
			for _, h := range out.Holds {
				if h.CustomerID != customer {
					continue
				}
				for _, l := range h.Limits {
					if l.Limit != "daily" {
						continue
					}
					if !lost && l.Held < 40 && version > 0 {
						lost = true
						return true
					}
					if !lost {
						version = h.Version
					}
				}
			}
			return false
		}
		holder := newGate(t, client, openrails.GateDeps{})
		require.NoError(t, gateAdmit(t, holder, gateWork(customer, "big", 40)).Release(t.Context()))
		require.EqualValues(t, 40, customerLimit(t, a.alpha, customer, "daily").Held)

		// Another gate wants more than is free: the holder's idle hold shrinks
		// on its next sync, whose reply is lost.
		contender := newGate(t, a.alpha, openrails.GateDeps{})
		patient, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		ticket, err := contender.Admit(patient, gateWork(customer, "contender", 30))
		require.NoError(t, err)
		require.True(t, ticket.Allowed(), "the shrunk capacity reaches the contender once the holder applied it: %+v", ticket.Refusal())
		mu.Lock()
		defer mu.Unlock()
		require.True(t, lost, "the shrink's reply was lost")
		require.True(t, checked)
		require.NoError(t, readErr)
		for _, applied := range afterLoss {
			require.False(t, applied.CustomerID == customer && applied.Version > version, "the gate acknowledged a shrink it never received: %+v", applied)
		}
		require.EqualValues(t, 40, heldAfterLoss, "a shrink the gate never applied releases nothing")
	})

	t.Run("OR-J23-S30/a-lease-answered-dead-is-closed-and-its-holds-free-at-once", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		// The first mode moves the server's clock past real time, so the
		// second's windows open ahead of its gates' clocks.
		for _, mode := range []string{"in process", "redis"} {
			customer := gateCustomer(t, a)
			client, wire := gateRemote(t, a)
			var mu sync.Mutex
			var first billing.LeaseID
			dead, closed := false, false
			wire.watch = func(p billing.SyncLeasesParams, out *billing.SyncLeasesResult) bool {
				mu.Lock()
				defer mu.Unlock()
				if first.IsZero() {
					first = p.LeaseID
				}
				if p.LeaseID == first && !p.Close && !out.Lease.Live {
					dead = true
				}
				if p.LeaseID == first && p.Close && dead {
					closed = true
				}
				return false
			}
			cfg := openrails.GateDeps{}
			if mode == "redis" {
				cfg.Redis = criticalRedis(t)
			}
			g := newGate(t, client, cfg)
			require.NoError(t, gateAdmit(t, g, gateWork(customer, "warm", 50)).Release(t.Context()))
			require.Positive(t, customerLimit(t, a.alpha, customer, "daily").Held, mode)
			// The server's clock passes the lease's life between two syncs: its
			// next answer reports the lease dead and renews nothing, and the
			// gate closes it.
			a.clock.Advance(limits.LeaseLifetime + time.Second)
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return closed
			}, 10*time.Second, 20*time.Millisecond, "%s: the lease answered dead is closed", mode)
			// Readers count a dead lease's holds for a grace past its life; a
			// closed one's hold nothing, with the clock still inside it.
			require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "%s: the closed lease holds nothing", mode)
			// The dead lease's capacity goes to another gate at once.
			other := newGate(t, a.alpha, openrails.GateDeps{})
			taken := gateAdmit(t, other, gateWork(customer, "other", 55))
			// The first gate decides from no hold of the dead lease: it wants
			// again, on a new lease, and is refused.
			short, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			stale, err := g.Admit(short, gateWork(customer, "stale", 10))
			cancel()
			require.NoError(t, err, mode)
			require.False(t, stale.Allowed(), "%s: a dead lease's hold never decides again", mode)
			require.NoError(t, taken.Capture(t.Context(), workUsage(55)))
			gateFlush(t, other)
			require.EqualValues(t, 55, customerLimit(t, a.alpha, customer, "daily").Used, mode)
		}
	})

	t.Run("OR-J23-S31/a-redis-usage-write-whose-reply-was-lost-posts-once-when-retried", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		client, _ := gateRemote(t, a)
		rdb := criticalRedis(t)
		lost := &loseUsageReplies{}
		rdb.AddHook(lost)
		g := newGate(t, client, openrails.GateDeps{Redis: rdb})
		_, err := g.Consume(t.Context(), gateWork(customer, "warm", 1))
		require.NoError(t, err)
		gateFlush(t, g)
		retried := gateAdmit(t, g, gateWork(customer, "retried", 2))
		settled := gateAdmit(t, g, gateWork(customer, "settled", 4))
		// A failed write marks Redis down for a sync interval; Flush then
		// returns at once.
		redisUp := func() { time.Sleep(limits.SyncInterval + 200*time.Millisecond) }

		// Every try of the write lands and loses its reply: the capture fails
		// and its ticket stays open. Retried while Redis counts as down, it
		// goes to Redis anyway, finds its write and adds nothing.
		lost.lose.Store(3)
		require.Error(t, retried.Capture(t.Context(), workUsage(2)))
		require.NoError(t, retried.Capture(t.Context(), workUsage(2)))
		redisUp()
		gateFlush(t, g)
		require.EqualValues(t, 3, customerLimit(t, a.alpha, customer, "daily").Used, "the retried capture posts once")

		// The write's key outlives its settlement: a retry after the lost
		// write settled adds nothing.
		lost.lose.Store(3)
		require.Error(t, settled.Capture(t.Context(), workUsage(4)))
		redisUp()
		gateFlush(t, g)
		require.EqualValues(t, 7, customerLimit(t, a.alpha, customer, "daily").Used, "the lost write was in Redis and settled")
		require.NoError(t, settled.Capture(t.Context(), workUsage(4)))
		gateFlush(t, g)
		require.EqualValues(t, 7, customerLimit(t, a.alpha, customer, "daily").Used, "a retry after settlement posts nothing again")

		// A Record is keyed by its source and source_id.
		record := billing.UsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "work",
			Source: "batch", SourceID: "job-1", Dimensions: map[string]int64{"units": 5}, Amount: ptr(int64(0))}
		require.NoError(t, g.Record(t.Context(), record))
		require.NoError(t, g.Record(t.Context(), record))
		gateFlush(t, g)
		require.EqualValues(t, 12, customerLimit(t, a.alpha, customer, "daily").Used, "a repeated Record posts once")
	})
}

// loseUsageReplies lets a gate's usage writes run in Redis, then loses the
// replies of the next lose of them, as a connection that drops after the
// write.
type loseUsageReplies struct{ lose atomic.Int64 }

func (h *loseUsageReplies) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *loseUsageReplies) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *loseUsageReplies) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err != nil || !usageWrite(cmd) {
			return err
		}
		for {
			n := h.lose.Load()
			if n <= 0 {
				return nil
			}
			if h.lose.CompareAndSwap(n, n-1) {
				break
			}
		}
		lostErr := errors.New("critical fixture lost Redis's reply to a usage write")
		cmd.SetErr(lostErr)
		return lostErr
	}
}

// usageWrite reports the gate's usage script, the one script with a write
// key (a "}:k:" key).
func usageWrite(cmd redis.Cmder) bool {
	if name := cmd.Name(); name != "evalsha" && name != "eval" {
		return false
	}
	for _, arg := range cmd.Args() {
		if s, ok := arg.(string); ok && strings.Contains(s, "}:k:") {
			return true
		}
	}
	return false
}
