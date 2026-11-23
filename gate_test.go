package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// fakeSync speaks the lease-sync wire with simple sizing: a want grants
// size(cost) of each limit, capped by what is free; usage settles in order
// and consumes the lease's hold. Limits count the event's "units"; with
// money, balance counts 1000 micros per unit.
type fakeSync struct {
	mu        sync.Mutex
	capacity  map[string]int64
	balance   int64
	money     bool
	safe      *int64
	size      func(cost int64) int64
	settled   map[fakeLine]int64
	holds     map[fakeHold]map[string]int64
	leases    map[billing.LeaseID]*fakeLease
	down      bool
	dropReply int
	capHeld   int
	dead      map[billing.LeaseID]bool
	calls     int
	wants     int
	returns   []billing.HoldReturn
	firstSeen []int64
	clients   []*Client
}

type fakeLine struct {
	customer billing.CustomerID
	limit    string
}

type fakeHold struct {
	lease    billing.LeaseID
	customer billing.CustomerID
}

type fakeLease struct {
	through int64
	items   map[int64]string
}

const fakeSet = "standard"

func newFakeSync(capacity map[string]int64) *fakeSync {
	return &fakeSync{capacity: capacity, settled: map[fakeLine]int64{}, holds: map[fakeHold]map[string]int64{},
		leases: map[billing.LeaseID]*fakeLease{}, dead: map[billing.LeaseID]bool{}}
}

func (f *fakeSync) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return nil, errors.New("fake: connection refused")
	}
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func (f *fakeSync) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewRemoteMerchant("http://openrails.test", RemoteCredential{APIKey: "test-key"}, RemoteMerchantOptions{HTTPClient: &http.Client{Transport: f}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pool makes n clients outside a synctest bubble: a client starts its read
// cache's goroutines when made.
func (f *fakeSync) pool(t *testing.T, n int) {
	for range n {
		f.clients = append(f.clients, f.client(t))
	}
}

func (f *fakeSync) limitsOf() []string {
	keys := make([]string, 0, len(f.capacity)+1)
	for k := range f.capacity {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if f.money {
		keys = append(keys, "balance")
	}
	return keys
}

func (f *fakeSync) capOf(limit string) int64 {
	if limit == "balance" {
		return f.balance
	}
	return f.capacity[limit]
}

func fakeCost(limit string, units int64) int64 {
	if limit == "balance" {
		return units * 1000
	}
	return units
}

func (f *fakeSync) heldBy(customer billing.CustomerID, limit string, except billing.LeaseID) (mine, others int64) {
	for k, h := range f.holds {
		if k.customer != customer {
			continue
		}
		if k.lease == except {
			mine += h[limit]
		} else {
			others += h[limit]
		}
	}
	return mine, others
}

func (f *fakeSync) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/app/leases/sync" {
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		return
	}
	var p billing.SyncLeasesParams
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeFakeError(w, 400, billing.CodeInvalidParam, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	lease := f.leases[p.LeaseID]
	if lease == nil {
		lease = &fakeLease{items: map[int64]string{}}
		f.leases[p.LeaseID] = lease
	}
	touched := map[billing.CustomerID]bool{}
	res := billing.SyncLeasesResult{Lease: billing.Lease{ID: p.LeaseID, ExpiresAt: time.Now().Add(10 * time.Second).UTC()}, Wants: []billing.LeaseWantResult{}, Holds: []billing.Hold{}, Limits: []billing.CustomerLimit{}}
	for _, u := range p.Usage {
		n := u.N
		u.N = 0
		body, _ := json.Marshal(u)
		switch {
		case n <= lease.through:
			if lease.items[n] != string(body) {
				res.Usage.Refused = append(res.Usage.Refused, billing.LeaseUsageRefusal{N: n, Status: 422, Error: &billing.ErrorDetails{Code: billing.CodeIdempotencyKeyReused}})
			}
			continue
		case n != lease.through+1:
			writeFakeError(w, 409, billing.CodeUsageOutOfOrder, fmt.Sprintf("usage %d after %d", n, lease.through))
			return
		}
		lease.through, lease.items[n] = n, string(body)
		f.firstSeen = append(f.firstSeen, n)
		touched[u.CustomerID] = true
		units := u.Dimensions["units"]
		for _, l := range f.limitsOf() {
			c := fakeCost(l, units)
			f.settled[fakeLine{u.CustomerID, l}] += c
			if h := f.holds[fakeHold{p.LeaseID, u.CustomerID}]; h != nil {
				h[l] -= min(c, h[l])
			}
		}
	}
	res.Usage.ThroughN = lease.through
	for _, ret := range p.Returns {
		f.returns = append(f.returns, ret)
		delete(f.holds, fakeHold{p.LeaseID, ret.CustomerID})
	}
	committed := map[fakeLine]int64{}
	for _, c := range p.Committed {
		for _, l := range c.Limits {
			committed[fakeLine{c.CustomerID, l.Limit}] += l.Committed
		}
	}
	for _, want := range p.Wants {
		f.wants++
		touched[want.CustomerID] = true
		res.Wants = append(res.Wants, f.want(p.LeaseID, want, committed))
	}
	for k, h := range f.holds {
		if k.lease == p.LeaseID && touched[k.customer] {
			res.Holds = append(res.Holds, f.hold(k.customer, h))
		}
	}
	for customer := range touched {
		for _, l := range f.limitsOf() {
			mine, others := f.heldBy(customer, l, p.LeaseID)
			used := f.settled[fakeLine{customer, l}]
			unit := "units"
			if l == "balance" {
				unit = "USD"
			}
			res.Limits = append(res.Limits, billing.CustomerLimit{CustomerID: customer, Limit: l, Unit: unit, Capacity: f.capOf(l), Used: used, Held: mine + others, Remaining: f.capOf(l) - used - mine - others})
		}
	}
	if f.dropReply > 0 {
		f.dropReply--
		writeFakeError(w, 503, "unavailable", "reply lost")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (f *fakeSync) want(lease billing.LeaseID, want billing.LeaseWant, committed map[fakeLine]int64) billing.LeaseWantResult {
	if f.dead[lease] {
		return billing.LeaseWantResult{Status: 409, Error: &billing.ErrorDetails{Code: billing.CodeLeaseNotHeld, Message: "lease ended"}}
	}
	set := fakeSet
	if f.capHeld > 0 {
		f.capHeld--
		return billing.LeaseWantResult{Status: 429, LimitSet: &set, Refusal: capacityHeld(1)}
	}
	var units int64
	if want.Usage != nil {
		units = want.Usage.Dimensions["units"]
	}
	k := fakeHold{lease, want.CustomerID}
	grant := map[string]int64{}
	for _, l := range f.limitsOf() {
		need := fakeCost(l, units)
		mine, others := f.heldBy(want.CustomerID, l, lease)
		free := f.capOf(l) - f.settled[fakeLine{want.CustomerID, l}] - mine - others
		uncommitted := mine - committed[fakeLine{want.CustomerID, l}]
		short := max(need-uncommitted, 0)
		if free < short {
			if free+others >= short {
				return billing.LeaseWantResult{Status: 429, LimitSet: &set, Refusal: capacityHeld(1)}
			}
			reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			limit := l
			return billing.LeaseWantResult{Status: 429, LimitSet: &set, Refusal: &billing.Refusal{DenyCode: billing.DenyLimitReached, Limit: &limit, ResetsAt: &reset}}
		}
		size := need
		if f.size != nil {
			size = f.size(need)
		}
		grant[l] = mine + min(free, max(size-uncommitted, short))
	}
	if f.holds[k] == nil {
		f.holds[k] = map[string]int64{}
	}
	for l, v := range grant {
		f.holds[k][l] = v
	}
	return billing.LeaseWantResult{Status: 200, LimitSet: &set}
}

func (f *fakeSync) hold(customer billing.CustomerID, held map[string]int64) billing.Hold {
	h := billing.Hold{CustomerID: customer, LimitSet: fakeSet, Currency: "USD", Limits: []billing.HoldLimit{}, RateCards: []billing.HoldRateCard{}, PriceBounds: []billing.PriceBound{}, Safe: f.safe}
	unit := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1, DivideBy: 1}}
	for _, l := range f.limitsOf() {
		if l == "balance" {
			h.Limits = append(h.Limits, billing.HoldLimit{Limit: l, Unit: "USD", Held: held[l], Overrun: catalog.OverrunRequireEstimate})
			continue
		}
		limit := l
		h.Limits = append(h.Limits, billing.HoldLimit{Limit: l, Unit: "units", Held: held[l], Overrun: catalog.OverrunRequireEstimate})
		h.RateCards = append(h.RateCards, billing.HoldRateCard{Limit: &limit, Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum, Price: unit})
	}
	if f.money {
		h.RateCards = append(h.RateCards, billing.HoldRateCard{Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum,
			Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1000, DivideBy: 1}}})
		h.PriceBounds = append(h.PriceBounds, billing.PriceBound{Meter: "work", UnitAmount: 1000, DivideBy: 1})
	}
	return h
}

func writeFakeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Type: "api_error", Code: code, Message: msg}})
}

func (f *fakeSync) set(fn func(f *fakeSync)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSync) settledOf(customer billing.CustomerID, limit string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settled[fakeLine{customer, limit}]
}

func work(customer billing.CustomerID, id string, units int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": units}}}
}

func capture(units int64) billing.CaptureUsage {
	return billing.CaptureUsage{Dimensions: map[string]int64{"units": units}}
}

func newTestGate(t *testing.T, f *fakeSync, cfg GateConfig) *Gate {
	t.Helper()
	f.mu.Lock()
	if len(f.clients) == 0 {
		f.mu.Unlock()
		t.Fatal("no client left: create them outside the bubble with pool")
	}
	c := f.clients[0]
	f.clients = f.clients[1:]
	f.mu.Unlock()
	g, err := NewGate(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func closeGate(t *testing.T, g *Gate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = g.Close(ctx)
}

func mustAdmit(t *testing.T, g *Gate, p billing.AdmitParams) *Ticket {
	t.Helper()
	ticket, err := g.Admit(t.Context(), p)
	if err != nil {
		t.Fatalf("admit %s: %v", p.RequestID, err)
	}
	if !ticket.Allowed() {
		t.Fatalf("admit %s refused: %+v", p.RequestID, ticket.Refusal())
	}
	return ticket
}

func flush(t *testing.T, g *Gate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := g.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestGateRacesNeverExceedEitherHold(t *testing.T) {
	f := newFakeSync(map[string]int64{"hourly": 100, "daily": 60})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(cost int64) int64 { return 4 * cost }
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		var mu sync.Mutex
		admitted := int64(0)
		var wg sync.WaitGroup
		for w := range 16 {
			wg.Go(func() {
				for i := range 10 {
					units := int64(1 + (w+i)%3)
					ticket, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("r-%d-%d", w, i), units))
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
					if err := ticket.Capture(t.Context(), capture(units)); err != nil {
						t.Errorf("capture: %v", err)
					}
				}
			})
		}
		wg.Wait()
		flush(t, g)
		if daily := f.settledOf(customer, "daily"); daily != admitted || daily > 60 {
			t.Fatalf("daily settled %d, admitted %d: never past the 60 held", daily, admitted)
		}
		if hourly := f.settledOf(customer, "hourly"); hourly != admitted {
			t.Fatalf("hourly settled %d, admitted %d", hourly, admitted)
		}
		if admitted < 58 {
			t.Fatalf("admitted %d of 60: the gate refused capacity it could have held", admitted)
		}
	})
}

func TestGateHoldingEightyAndCapturingThirtyLeavesSeventy(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		ticket := mustAdmit(t, g, work(customer, "eighty", 80))
		if err := ticket.Capture(t.Context(), capture(30)); err != nil {
			t.Fatal(err)
		}
		if err := ticket.Release(t.Context()); err != nil {
			t.Fatal("release after capture is a no-op:", err)
		}
		lines, err := g.Limits(t.Context(), customer)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 1 || lines[0].Used != 30 {
			t.Fatalf("display before the sync %+v: unsent usage counts as used", lines)
		}
		mustAdmit(t, g, work(customer, "seventy", 70))
		refused, err := g.Admit(t.Context(), work(customer, "one-more", 1))
		if err != nil || refused.Allowed() || refused.Refusal().DenyCode != billing.DenyLimitReached {
			t.Fatalf("one past the 70 left: %+v, %v", refused.Refusal(), err)
		}
		r := refused.Refusal()
		if r.Limit == nil || *r.Limit != "quota" || r.ResetsAt == nil {
			t.Fatalf("limit_reached names the limit and its reset: %+v", r)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 30 {
			t.Fatalf("settled %d, want 30", got)
		}
	})
}

func TestGateNumbersUsageOnceAndResendsAfterALostReply(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 1000})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(cost int64) int64 { return 100 }
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		for i := range 5 {
			if _, err := g.Consume(t.Context(), work(customer, fmt.Sprintf("c-%d", i), 2)); err != nil {
				t.Fatal(err)
			}
		}
		f.set(func(f *fakeSync) { f.dropReply = 1 })
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := g.Flush(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("flush with its reply lost = %v, want ErrUnavailable", err)
		}
		flush(t, g)
		for i := range 3 {
			if err := g.Record(t.Context(), billing.UsageParams{CustomerID: customer, Currency: "USD", EventType: "work", Dimensions: map[string]int64{"units": 1}, Source: "batch", SourceID: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 13 {
			t.Fatalf("settled %d, want 13: each item once", got)
		}
		f.mu.Lock()
		seen := slices.Clone(f.firstSeen)
		f.mu.Unlock()
		if !slices.Equal(seen, []int64{1, 2, 3, 4, 5, 6, 7, 8}) {
			t.Fatalf("items settled in order %v, want 1..8 once each", seen)
		}
	})
}

func TestGateWaitsOutCapacityHeld(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.capHeld = 2
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		started := time.Now()
		mustAdmit(t, g, work(customer, "patient", 1))
		if waited := time.Since(started); waited < 2*time.Second {
			t.Fatalf("admitted after %v: two capacity_held answers are waited out", waited)
		}
		f.set(func(f *fakeSync) { f.capHeld = 100 })
		short, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()
		ticket, err := g.Admit(short, work(customer, "impatient", 20))
		if err != nil {
			t.Fatal(err)
		}
		if ticket.Allowed() || ticket.Refusal().DenyCode != billing.DenyCapacityHeld {
			t.Fatalf("a request whose context ends is refused capacity_held: %+v", ticket.Refusal())
		}
	})
}

func TestGateDecidesWarmWhileUnreachableAndSpendsSafe(t *testing.T) {
	f := newFakeSync(map[string]int64{})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.money, f.balance = true, 100_000
		f.size = func(cost int64) int64 { return 3 * cost }
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		var calls, after int
		f.set(func(f *fakeSync) { f.down, calls = true, f.calls })
		started := time.Now()
		mustAdmit(t, g, work(customer, "warm", 3))
		f.set(func(f *fakeSync) { after = f.calls })
		if time.Since(started) != 0 || after != calls {
			t.Fatal("a warm decision waits for nothing")
		}
		_, err := g.Admit(t.Context(), work(customer, "short", 1))
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, billing.ErrUnreachable) {
			t.Fatalf("a short hold with OpenRails down = %v, want ErrUnavailable", err)
		}
		_, err = g.Admit(t.Context(), work(billing.CustomerID(uuid.New()), "cold", 1))
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a cold customer with OpenRails down = %v, want ErrUnavailable", err)
		}

		safe := int64(5000)
		f.set(func(f *fakeSync) { f.down, f.safe = false, &safe })
		time.Sleep(1100 * time.Millisecond)
		mustAdmit(t, g, work(customer, "renew", 3)).Release(t.Context())
		f.set(func(f *fakeSync) { f.down = true })
		time.Sleep(1100 * time.Millisecond)
		avail := func() int64 {
			p := work(customer, "probe", 0)
			if err := normalizeGateAdmit(&p); err != nil {
				t.Fatal(err)
			}
			h, _ := g.local.holdFor(attrsKey(p), customer)
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.parts[partID("balance", nil)].avail()
		}
		mustAdmit(t, g, work(customer, "from-hold", avail()/1000))
		mustAdmit(t, g, work(customer, "from-safe", 4))
		if _, err := g.Admit(t.Context(), work(customer, "past-safe", 2)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("past the safe amount = %v, want ErrUnavailable", err)
		}
		time.Sleep(11 * time.Second)
		mustAdmit(t, g, work(customer, "dead-lease", 1))
		if _, err := g.Admit(t.Context(), work(customer, "safe-spent", 1)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a dead lease's hold past its safe amount = %v, want ErrUnavailable", err)
		}
	})
}

func TestGateSpendsSafeOnMoneyNeverOnUnits(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		safe := int64(50_000)
		f.money, f.balance, f.safe = true, 100_000, &safe
		f.size = func(cost int64) int64 { return 2 * cost }
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		stated := func(id string, amount int64) billing.AdmitParams {
			return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD", EstimatedAmount: amount}
		}
		mustAdmit(t, g, work(customer, "warm-up", 2)).Release(t.Context())
		f.set(func(f *fakeSync) { f.down = true })
		mustAdmit(t, g, work(customer, "from-hold", 4))
		if _, err := g.Admit(t.Context(), work(customer, "past-units", 1)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("units past the hold with OpenRails down = %v, want ErrUnavailable", err)
		}
		mustAdmit(t, g, stated("money-from-safe", 3000))
		time.Sleep(11 * time.Second)
		if _, err := g.Admit(t.Context(), work(customer, "dead-lease-units", 1)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("units on a dead lease = %v, want ErrUnavailable", err)
		}
		mustAdmit(t, g, stated("dead-lease-money", 3000))
	})
}

func TestGateReturnsLeastRecentlyUsedHolds(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		g.local.mu.Lock()
		g.local.maxHolds = 2
		g.local.mu.Unlock()
		customers := []billing.CustomerID{billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())}
		for i, c := range customers {
			if _, err := g.Consume(t.Context(), work(c, fmt.Sprint(i), 1)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		flush(t, g)
		time.Sleep(2 * time.Second)
		f.mu.Lock()
		returns := slices.Clone(f.returns)
		f.mu.Unlock()
		if len(returns) != 1 || returns[0].CustomerID != customers[0] || returns[0].LimitSet != fakeSet {
			t.Fatalf("returns %+v, want the least recently used customer's hold", returns)
		}
	})
}

func TestGateTicketLivesAcrossSyncs(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		ticket := mustAdmit(t, g, work(customer, "long", 4))
		f.mu.Lock()
		calls := f.calls
		f.mu.Unlock()
		time.Sleep(30 * time.Second)
		f.mu.Lock()
		synced := f.calls - calls
		f.mu.Unlock()
		if synced < 25 {
			t.Fatalf("%d syncs in 30s: an idle gate keeps its lease alive", synced)
		}
		if err := ticket.Capture(t.Context(), capture(6)); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 6 {
			t.Fatalf("settled %d, want 6: the capture past its hold posts in full", got)
		}
	})
}

func TestGateRollsOverADeadLease(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		ticket := mustAdmit(t, g, work(customer, "first", 10))
		first := g.local.lease
		f.set(func(f *fakeSync) { f.dead[first] = true })
		if err := ticket.Capture(t.Context(), capture(2)); err != nil {
			t.Fatal(err)
		}
		mustAdmit(t, g, work(billing.CustomerID(uuid.New()), "other", 1))
		if g.local.lease == first {
			t.Fatal("a lease the server no longer holds was kept")
		}
		flush(t, g)
		f.mu.Lock()
		through := f.leases[first].through
		f.mu.Unlock()
		if through != 1 {
			t.Fatalf("the old lease settled through %d, want its one item", through)
		}
	})
}

// memOutbox is an in-memory UsageOutbox.
type memOutbox struct {
	mu    sync.Mutex
	items map[billing.LeaseUsageRef]billing.LeaseUsage
}

func (o *memOutbox) Put(_ context.Context, items []OutboxItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range items {
		o.items[billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}] = it.Usage
	}
	return nil
}

func (o *memOutbox) Pending(context.Context) ([]OutboxItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]OutboxItem, 0, len(o.items))
	for ref, u := range o.items {
		out = append(out, OutboxItem{LeaseID: ref.LeaseID, Usage: u})
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

// crash stops a gate's sync loop without sending anything.
func crash(g *Gate) {
	g.local.mu.Lock()
	g.local.stopped = true
	g.local.mu.Unlock()
	close(g.local.stop)
	<-g.local.done
}

func TestGateRestartResendsTheOutboxAndForgetsMemory(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		outbox := &memOutbox{items: map[billing.LeaseUsageRef]billing.LeaseUsage{}}
		customer := billing.CustomerID(uuid.New())

		durable := newTestGate(t, f, GateConfig{Outbox: outbox})
		plain := newTestGate(t, f, GateConfig{})
		for i := range 3 {
			mustAdmit(t, durable, work(customer, fmt.Sprintf("d-%d", i), 2))
		}
		mustAdmit(t, plain, work(customer, "p", 2))
		f.set(func(f *fakeSync) { f.down = true })
		for _, g := range []*Gate{durable, plain} {
			if _, err := g.Consume(t.Context(), work(customer, "lost-"+fmt.Sprint(g == plain), 2)); err != nil {
				t.Fatal(err)
			}
		}
		lease := durable.local.lease
		crash(durable)
		crash(plain)
		if len(outbox.items) != 1 {
			t.Fatalf("outbox holds %d items, want the unsent capture", len(outbox.items))
		}
		f.set(func(f *fakeSync) { f.down = false })
		restarted := newTestGate(t, f, GateConfig{Outbox: outbox})
		defer closeGate(t, restarted)
		time.Sleep(2 * time.Second)
		f.mu.Lock()
		through := f.leases[lease].through
		f.mu.Unlock()
		if through != 1 || f.settledOf(customer, "quota") != 2 {
			t.Fatalf("the restarted gate settled %d of the dead lease, quota %d: want its one unsent item, once", through, f.settledOf(customer, "quota"))
		}
		outbox.mu.Lock()
		left := len(outbox.items)
		outbox.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d outbox items left after settlement", left)
		}
	})
}

func TestGateCloseSendsUsageAndReturnsHolds(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 10 }
		g := newTestGate(t, f, GateConfig{})
		a, b := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		for i, c := range []billing.CustomerID{a, b} {
			if _, err := g.Consume(t.Context(), work(c, fmt.Sprint(i), 3)); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		returns, calls := len(f.returns), f.calls
		f.mu.Unlock()
		if f.settledOf(a, "quota") != 3 || f.settledOf(b, "quota") != 3 || returns != 2 {
			t.Fatalf("close settled %d and %d, returned %d holds", f.settledOf(a, "quota"), f.settledOf(b, "quota"), returns)
		}
		time.Sleep(3 * time.Second)
		f.mu.Lock()
		after := f.calls
		f.mu.Unlock()
		if after != calls {
			t.Fatal("a closed gate stops syncing")
		}
		if _, err := g.Admit(t.Context(), work(a, "late", 1)); !errors.Is(err, errGateClosed) {
			t.Fatalf("admit on a closed gate = %v", err)
		}
	})
}

func TestGateRefusesBadInput(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateConfig{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		bad := work(customer, "", 1)
		if _, err := g.Admit(t.Context(), bad); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("no request id = %v", err)
		}
		both := work(customer, "both", 1)
		both.EstimatedAmount = 5
		if _, err := g.Admit(t.Context(), both); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("usage and an amount = %v", err)
		}
		ticket := mustAdmit(t, g, work(customer, "ok", 1))
		if err := ticket.Capture(t.Context(), billing.CaptureUsage{EventType: "other"}); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("capture of another event type = %v", err)
		}
		if err := ticket.Capture(t.Context(), capture(1)); err != nil {
			t.Fatalf("the refused capture left the ticket open: %v", err)
		}
		if err := ticket.Capture(t.Context(), capture(1)); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("second capture = %v", err)
		}
	})
	rdb := nilRedisForTest()
	defer rdb.Close()
	if _, err := NewGate(newFakeSync(nil).client(t), GateConfig{Outbox: &memOutbox{}, Redis: rdb}); err == nil {
		t.Fatal("Redis and an outbox together are refused")
	}
}

func nilRedisForTest() redis.UniversalClient {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
}
