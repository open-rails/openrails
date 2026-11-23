//go:build integration

package critical_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// gateWire is a remote gate's transport to OpenRails: it counts lease
// syncs, can be cut, and can lose a sync's reply after the server answered.
type gateWire struct {
	http.RoundTripper
	syncs     atomic.Int64
	down      atomic.Bool
	dropReply atomic.Int64
}

func (w *gateWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if w.down.Load() {
		return nil, errors.New("critical fixture cannot reach OpenRails")
	}
	if r.URL.Path != "/v1/app/leases/sync" {
		return w.RoundTripper.RoundTrip(r)
	}
	w.syncs.Add(1)
	resp, err := w.RoundTripper.RoundTrip(r)
	if err == nil && w.dropReply.Load() > 0 {
		w.dropReply.Add(-1)
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

func newGate(t *testing.T, client *openrails.Client, cfg openrails.GateConfig) *openrails.Gate {
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

func (o *memOutbox) Put(_ context.Context, items []openrails.OutboxItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range items {
		o.items[billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}] = it.Usage
	}
	return nil
}

func (o *memOutbox) Pending(context.Context) ([]openrails.OutboxItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]openrails.OutboxItem, 0, len(o.items))
	for ref, u := range o.items {
		out = append(out, openrails.OutboxItem{LeaseID: ref.LeaseID, Usage: u})
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
			g := newGate(t, client, openrails.GateConfig{})
			gateAdmit(t, g, gateWork(customer, name+"-cold", 2)).Release(t.Context())
			for i := range 5 {
				_, err := g.Consume(t.Context(), gateWork(customer, fmt.Sprintf("%s-%d", name, i), 1))
				require.NoError(t, err)
			}
			gateFlush(t, g)
			require.EqualValues(t, 5, customerLimit(t, a.alpha, customer, "daily").Used, "%s: each consumed unit settles once", name)
		}

		customer := gateCustomer(t, a)
		g := newGate(t, remoteClient, openrails.GateConfig{})
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
		g := newGate(t, a.alpha, openrails.GateConfig{})
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
		g := newGate(t, client, openrails.GateConfig{})
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
		durable := newGate(t, durableClient, openrails.GateConfig{Outbox: outbox})
		plainClient, plainWire := gateRemote(t, a)
		plain := newGate(t, plainClient, openrails.GateConfig{})
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
		restarted := newGate(t, restartedClient, openrails.GateConfig{Outbox: outbox})
		require.Eventually(t, func() bool { return outbox.len() == 0 }, 10*time.Second, 50*time.Millisecond, "the restarted process resends and acknowledges")
		require.EqualValues(t, 11, customerLimit(t, a.alpha, customer, "daily").Used, "the outbox's 3 units settle once; the 5 without one never")

		// The stopped processes' leases die and their holds never reopen.
		a.clock.Advance(11 * time.Second)
		gateFlush(t, restarted)
		_, err = restarted.Consume(t.Context(), gateWork(customer, "after", 1))
		require.NoError(t, err)
		gateFlush(t, restarted)
		daily := customerLimit(t, a.alpha, customer, "daily")
		require.EqualValues(t, 12, daily.Used)
		require.NoError(t, restarted.Close(t.Context()))
		a.clock.Advance(11 * time.Second)
		require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "no dead lease holds capacity")
	})

	t.Run("OR-J23-S22/a-ticket-lives-across-syncs-and-captures-across-a-session-reset", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, tokenPlan)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, a.alpha, customer, "pro", 24*30)
		g := newGate(t, a.alpha, openrails.GateConfig{})
		chat := func(id string, tokens int64) billing.AdmitParams {
			return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
				Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"input_tokens": tokens}, Metadata: map[string]any{"model": "large"}, Amount: ptr(int64(0))}}
		}
		capture := func(tokens int64) billing.CaptureUsage {
			return billing.CaptureUsage{Dimensions: map[string]int64{"input_tokens": tokens}, Metadata: map[string]any{"model": "large"}}
		}

		long := gateAdmit(t, g, chat("long", 20_000))
		time.Sleep(3500 * time.Millisecond)
		require.NoError(t, long.Capture(t.Context(), capture(40_000)), "a ticket outlives many syncs with no deadline")
		gateFlush(t, g)
		require.EqualValues(t, 200, customerLimit(t, a.alpha, customer, "five_hour").Used)

		late := gateAdmit(t, g, chat("late", 20_000))
		a.clock.Advance(5*time.Hour + time.Minute)
		require.NoError(t, late.Capture(t.Context(), capture(20_000)))
		gateFlush(t, g)
		require.Nil(t, customerLimit(t, a.alpha, customer, "five_hour").WindowStartsAt, "the capture settles in its ended session and opens none")
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
			return process{gate: newGate(t, client, openrails.GateConfig{Redis: r}), wire: wire, redis: r}
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
		a.clock.Advance(11 * time.Second)
		require.Zero(t, customerLimit(t, a.alpha, customer, "daily").Held, "the expired group lease's capacity came back")
	})

	t.Run("OR-J23-S24/refusals-are-verdicts-and-openrails-down-is-unavailable", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alpha, gatePlan)
		customer := gateCustomer(t, a)
		g := newGate(t, a.alpha, openrails.GateConfig{})
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
		holder := newGate(t, a.alpha, openrails.GateConfig{})
		contender := newGate(t, a.alpha, openrails.GateConfig{})
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
		down := newGate(t, client, openrails.GateConfig{})
		wire.down.Store(true)
		_, err = down.Admit(t.Context(), gateWork(gateCustomer(t, a), "down", 1))
		require.ErrorIs(t, err, openrails.ErrUnavailable)
		require.ErrorIs(t, err, billing.ErrUnreachable)
		wire.down.Store(false)
	})
}
