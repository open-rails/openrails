package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// fakeSync speaks the lease-sync wire with simple sizing: a want grants
// size(cost) of each limit, capped by what is free; usage settles in order
// and consumes the lease's hold. Limits count the event's "units"; with
// money, balance counts 1000 micros per unit.
type fakeSync struct {
	mu        sync.Mutex
	merchant  billing.MerchantID
	capacity  map[string]int64
	balance   int64
	money     bool
	safe      *int64
	size      func(cost int64) int64
	settled   map[fakeLine]int64
	holds     map[fakeHold]map[string]int64
	versions  map[fakeHold]int64
	leases    map[billing.LeaseID]*fakeLease
	down      bool
	dropReply int
	reject    int
	stall     bool
	perSync   int // new items one sync settles, 0 every one, as a server settling a prefix
	noHolds   bool
	block     chan struct{}
	capHeld   int
	deferred  int // wants answered 503 service_unavailable, as by a sync out of time
	dead      map[billing.LeaseID]bool
	calls     int
	wants     int
	sentWants []billing.LeaseWant
	applied   [][]billing.HoldApplied
	returns   []billing.HoldReturn
	closed    []billing.LeaseID
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
	return &fakeSync{merchant: billing.MerchantID(uuid.New()), capacity: capacity, settled: map[fakeLine]int64{}, holds: map[fakeHold]map[string]int64{}, versions: map[fakeHold]int64{},
		leases: map[billing.LeaseID]*fakeLease{}, dead: map[billing.LeaseID]bool{}}
}

func (f *fakeSync) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	down, block := f.down, f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if down {
		return nil, errors.New("fake: connection refused")
	}
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func (f *fakeSync) client(t *testing.T) *Client {
	t.Helper()
	remote, err := NewRemote("http://openrails.test", RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{f.merchant: {APIKey: "test-key"}}, HTTPClient: &http.Client{Transport: f}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := remote.Merchant(f.merchant)
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
	w.Header().Set(merchant.BoundHeader, f.merchant.String())
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
	if f.reject != 0 {
		code := billing.ErrorCode("fake_rejected")
		if f.reject == http.StatusServiceUnavailable {
			// As a sync that could not take its locks, or commit, in time.
			code = billing.CodeServiceUnavailable
		}
		writeFakeError(w, f.reject, code, "rejected")
		return
	}
	f.applied = append(f.applied, p.Applied)
	f.sentWants = append(f.sentWants, p.Wants...)
	lease := f.leases[p.LeaseID]
	if lease == nil {
		lease = &fakeLease{items: map[int64]string{}}
		f.leases[p.LeaseID] = lease
	}
	live := !f.dead[p.LeaseID]
	touched := map[billing.CustomerID]bool{}
	res := billing.SyncLeasesResult{Lease: billing.Lease{ID: p.LeaseID, ExpiresAt: time.Now().Add(10 * time.Second).UTC(), Live: live},
		Usage: billing.LeaseUsageResult{Refused: []billing.LeaseUsageRefusal{}}, Wants: []billing.LeaseWantResult{}, Holds: []billing.Hold{}, Limits: []billing.CustomerLimit{}}
	fresh := 0
	for i, u := range p.Usage {
		if f.stall || f.perSync > 0 && fresh == f.perSync && u.N > lease.through {
			break
		}
		n := u.N
		u.N = 0
		body, _ := json.Marshal(u)
		if n <= lease.through {
			if lease.items[n] != string(body) {
				res.Usage.Refused = append(res.Usage.Refused, billing.LeaseUsageRefusal{N: n, Status: 422, Error: &billing.ErrorDetails{Code: billing.CodeIdempotencyKeyReused}})
			}
			continue
		}
		if n != lease.through+1 {
			for _, rest := range p.Usage[i:] {
				res.Usage.Refused = append(res.Usage.Refused, billing.LeaseUsageRefusal{N: rest.N, Status: 409, Error: &billing.ErrorDetails{Code: billing.CodeUsageOutOfOrder}})
			}
			break
		}
		lease.through, lease.items[n] = n, string(body)
		fresh++
		f.firstSeen = append(f.firstSeen, n)
		touched[u.CustomerID] = true
		units := u.Dimensions["units"]
		for _, l := range f.limitsOf() {
			c := fakeCost(l, units)
			f.settled[fakeLine{u.CustomerID, l}] += c
			k := fakeHold{p.LeaseID, u.CustomerID}
			if h := f.holds[k]; h != nil && c > 0 {
				h[l] -= min(c, h[l])
				f.versions[k]++
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
	if p.Close {
		f.closed = append(f.closed, p.LeaseID)
		f.dead[p.LeaseID] = true
		for k := range f.holds {
			if k.lease == p.LeaseID {
				delete(f.holds, k)
			}
		}
	}
	for _, want := range p.Wants {
		f.wants++
		if f.deferred > 0 {
			f.deferred--
			res.Wants = append(res.Wants, billing.LeaseWantResult{Status: http.StatusServiceUnavailable,
				Error: &billing.ErrorDetails{Code: billing.CodeServiceUnavailable, Message: "out of time"}})
			continue
		}
		touched[want.CustomerID] = true
		res.Wants = append(res.Wants, f.want(p.LeaseID, want, committed))
	}
	for k, h := range f.holds {
		if live && !f.noHolds && k.lease == p.LeaseID && touched[k.customer] {
			res.Holds = append(res.Holds, f.hold(k.customer, h, f.versions[k]))
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
		if l == "balance" && want.EstimatedAmount != nil {
			need = *want.EstimatedAmount
		}
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
	f.versions[k]++
	return billing.LeaseWantResult{Status: 200, LimitSet: &set}
}

func (f *fakeSync) hold(customer billing.CustomerID, held map[string]int64, version int64) billing.Hold {
	h := billing.Hold{CustomerID: customer, LimitSet: fakeSet, Currency: "USD", Limits: []billing.HoldLimit{}, RateCards: []billing.HoldRateCard{}, PriceBounds: []billing.PriceBound{}, Safe: f.safe, Version: version}
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

func writeFakeError(w http.ResponseWriter, status int, code billing.ErrorCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Type: billing.ErrorTypeAPI, Code: code, Message: msg}})
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

func newTestGate(t *testing.T, f *fakeSync, cfg GateDeps) *Gate {
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

func leaseOf(g *Gate) billing.LeaseID {
	g.local.mu.Lock()
	defer g.local.mu.Unlock()
	return g.local.lease
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
		g := newTestGate(t, f, GateDeps{})
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
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		ticket := mustAdmit(t, g, work(customer, "eighty", 80))
		if err := ticket.Capture(t.Context(), capture(30)); err != nil {
			t.Fatal(err)
		}
		if err := ticket.Release(t.Context()); err != nil {
			t.Fatal("release after capture is a no-op:", err)
		}
		lines, err := g.ListCustomerLimits(t.Context(), customer)
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
		g := newTestGate(t, f, GateDeps{})
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
		g := newTestGate(t, f, GateDeps{})
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

func TestGateSendsADeferredWantAgain(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		f.deferred = 2
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		lease := leaseOf(g)
		mustAdmit(t, g, work(billing.CustomerID(uuid.New()), "deferred", 1))
		f.mu.Lock()
		wants := f.wants
		f.mu.Unlock()
		if wants != 3 {
			t.Fatalf("a want the sync deferred is sent again until answered: %d sent", wants)
		}
		if leaseOf(g) != lease {
			t.Fatal("a deferred want is no reason to start a new lease")
		}
	})
}

func TestGateDecidesWarmWhileUnreachableAndSpendsSafe(t *testing.T) {
	f := newFakeSync(map[string]int64{})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.money, f.balance = true, 100_000
		f.size = func(cost int64) int64 { return 3 * cost }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		// A sync the warm-up started (its hold ran low) finishes first.
		synctest.Wait()
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
			h, _ := g.local.holdFor(attrsKey(p), customer, false)
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
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		stated := func(id string, amount int64) billing.AdmitParams {
			return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD", EstimatedAmount: amount}
		}
		mustAdmit(t, g, work(customer, "warm-up", 2)).Release(t.Context())
		mustAdmit(t, g, stated("warm-up-stated", 1000)).Release(t.Context())
		synctest.Wait()
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
		g := newTestGate(t, f, GateDeps{})
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
		g := newTestGate(t, f, GateDeps{})
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
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		ticket := mustAdmit(t, g, work(customer, "first", 10))
		first := leaseOf(g)
		f.set(func(f *fakeSync) { f.dead[first] = true })
		if err := ticket.Capture(t.Context(), capture(2)); err != nil {
			t.Fatal(err)
		}
		mustAdmit(t, g, work(billing.CustomerID(uuid.New()), "other", 1))
		if leaseOf(g) == first {
			t.Fatal("a lease the server no longer holds was kept")
		}
		flush(t, g)
		f.mu.Lock()
		through := f.leases[first].through
		f.mu.Unlock()
		if through != 1 {
			t.Fatalf("the old lease settled through %d, want its one item", through)
		}
		if uuid.UUID(leaseOf(g)).Version() != 7 {
			t.Fatalf("lease id %s is not a UUIDv7", leaseOf(g))
		}
	})
}

// memOutbox is an in-memory UsageOutbox.
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

		durable := newTestGate(t, f, GateDeps{Outbox: outbox})
		plain := newTestGate(t, f, GateDeps{})
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
		restarted := newTestGate(t, f, GateDeps{Outbox: outbox})
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
		g := newTestGate(t, f, GateDeps{})
		a, b := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		for i, c := range []billing.CustomerID{a, b} {
			if _, err := g.Consume(t.Context(), work(c, fmt.Sprint(i), 3)); err != nil {
				t.Fatal(err)
			}
		}
		lease := leaseOf(g)
		if err := g.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		returns, calls, closed := len(f.returns), f.calls, slices.Clone(f.closed)
		f.mu.Unlock()
		if f.settledOf(a, "quota") != 3 || f.settledOf(b, "quota") != 3 || returns != 2 {
			t.Fatalf("close settled %d and %d, returned %d holds", f.settledOf(a, "quota"), f.settledOf(b, "quota"), returns)
		}
		if len(closed) == 0 || closed[0] != lease {
			t.Fatalf("closed leases %v, want the gate's %s", closed, lease)
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
		g := newTestGate(t, f, GateDeps{})
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
	if _, err := NewGate(newFakeSync(nil).client(t), GateDeps{Outbox: &memOutbox{}, Redis: rdb}); err == nil {
		t.Fatal("Redis and an outbox together are refused")
	}
}

func nilRedisForTest() redis.UniversalClient {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
}

func dayHold(customer billing.CustomerID, start time.Time, daily, balance, version int64) billing.Hold {
	end := start.Add(24 * time.Hour)
	limit := "daily"
	return billing.Hold{CustomerID: customer, LimitSet: fakeSet, Currency: "USD", Version: version,
		Limits: []billing.HoldLimit{
			{Limit: "daily", Unit: "units", Held: daily, Overrun: catalog.OverrunRequireEstimate, WindowStartsAt: &start, WindowEndsAt: &end},
			{Limit: "balance", Unit: "USD", Held: balance, Overrun: catalog.OverrunRequireEstimate},
		},
		RateCards: []billing.HoldRateCard{
			{Limit: &limit, Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum,
				Price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1, DivideBy: 1}}},
			{Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum,
				Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 10, DivideBy: 1}}},
		},
		PriceBounds: []billing.PriceBound{{Meter: "work", UnitAmount: 10, DivideBy: 1}}}
}

func heldOf(h billing.Hold) map[string]int64 {
	out := map[string]int64{}
	for _, l := range h.Limits {
		out[partID(l.Limit, l.WindowStartsAt)] += l.Held
	}
	return out
}

func TestGateHoldSetMergesNextWindowHoldsByPartAndWindow(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	today, tomorrow := partID("daily", &t0), partID("daily", &t1)
	var set holdSet
	set.apply([]billing.Hold{dayHold(customer, t0, 10, 100, 3), dayHold(customer, t1, 5, 50, 4)}, t0.Add(time.Hour))
	if got := heldOf(set.merged()); got[today] != 10 || got[tomorrow] != 5 || got["balance@"] != 150 || set.Version != 4 {
		t.Fatalf("merged %v version %d: both windows kept, balance summed", got, set.Version)
	}
	// An answer of the next window alone leaves the current one as it was.
	set.apply([]billing.Hold{dayHold(customer, t1, 7, 60, 5)}, t0.Add(2*time.Hour))
	if got := heldOf(set.merged()); got[today] != 10 || got[tomorrow] != 7 || got["balance@"] != 160 {
		t.Fatalf("merged %v after a next-window answer", got)
	}
	set.consume("balance@", 30)
	if got := heldOf(set.merged()); got["balance@"] != 130 {
		t.Fatalf("balance %d after consuming 30 of 160", got["balance@"])
	}
	// Once today's window ends its hold is dropped.
	set.apply([]billing.Hold{dayHold(customer, t1, 7, 60, 6)}, t1.Add(time.Hour))
	if got := heldOf(set.merged()); got[today] != 0 || got["balance@"] != 60 {
		t.Fatalf("merged %v after today's window ended", got)
	}
	// Holds of one answer with the same windows add up.
	var twice holdSet
	twice.apply([]billing.Hold{dayHold(customer, t0, 10, 100, 1), dayHold(customer, t0, 10, 100, 2)}, t0)
	if got := heldOf(twice.merged()); got[today] != 20 || got["balance@"] != 200 {
		t.Fatalf("merged %v: two holds of one window add up", got)
	}
	if empty := (&holdSet{}).merged(); len(empty.Limits) != 0 {
		t.Fatalf("an empty set has parts %+v", empty.Limits)
	}
}

func TestGateDecidesFromTheCurrentWindowOfMergedHolds(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	h := newLocalHold(holdKey{customer, fakeSet}, newLeaseID())
	r := request{usage: &limits.Usage{EventType: "work", Dimensions: map[string]int64{"units": 3}}}
	if _, _, out, _ := h.decide(t0, r, true, false); out != decideCold {
		t.Fatalf("a hold without parts decided %d, want cold", out)
	}
	h.apply([]billing.Hold{dayHold(customer, t0, 10, 100, 1), dayHold(customer, t1, 5, 50, 2)}, t0)
	takes, _, out, err := h.decide(t0.Add(time.Hour), r, true, false)
	if err != nil || out != decideAllowed {
		t.Fatalf("decide = %d, %v", out, err)
	}
	got := map[string]int64{}
	for _, tk := range takes {
		got[tk.part] = tk.cost
	}
	if got[partID("daily", &t0)] != 3 || got["balance@"] != 30 || len(got) != 2 {
		t.Fatalf("takes %v: today's window and the money", got)
	}
	takes, _, out, _ = h.decide(t1.Add(time.Hour), r, true, false)
	if out != decideAllowed || takes[0].part != partID("daily", &t1) && takes[1].part != partID("daily", &t1) {
		t.Fatalf("tomorrow's request took %+v (%d), want tomorrow's window", takes, out)
	}
}

func TestGateDecidesFromAWindowTheServerOpenedAheadOfItsClock(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	opened := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	h := newLocalHold(holdKey{customer, fakeSet}, newLeaseID())
	h.apply([]billing.Hold{dayHold(customer, opened, 10, 100, 1)}, opened.Add(-time.Second))
	r := request{usage: &limits.Usage{EventType: "work", Dimensions: map[string]int64{"units": 3}}}
	// The server opened the window at its second; the gate's clock is behind.
	takes, _, out, err := h.decide(opened.Add(-500*time.Millisecond), r, true, false)
	if err != nil || out != decideAllowed {
		t.Fatalf("decide = %d, %v: the window the server holds is its current one", out, err)
	}
	got := map[string]int64{}
	for _, tk := range takes {
		got[tk.part] = tk.cost
	}
	if got[partID("daily", &opened)] != 3 || got["balance@"] != 30 || len(got) != 2 {
		t.Fatalf("takes %v: the opened window and the money", got)
	}
	if _, _, out, _ := h.decide(opened.Add(25*time.Hour), r, true, false); out != decideUnpriced {
		t.Fatalf("decide = %d once every window ended, want unpriced (a sync)", out)
	}
}

func TestGateLeavesADeadLeasesHoldsToSafeSpending(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		_ = mustAdmit(t, g, work(customer, "warm", 1)).Release(t.Context())
		synctest.Wait()
		p := work(customer, "probe", 1)
		if err := normalizeGateAdmit(&p); err != nil {
			t.Fatal(err)
		}
		attrs := attrsKey(p)
		warm, _ := g.local.holdFor(attrs, customer, false)
		if warm == nil {
			t.Fatal("no hold after a warm admit")
		}
		// OpenRails unreachable past the lease's life: the gate starts a new
		// lease and keeps the dead one's holds for safe spending alone.
		f.set(func(f *fakeSync) { f.down = true })
		time.Sleep(limits.LeaseLifetime + 2*limits.SyncInterval)
		synctest.Wait()
		if h, _ := g.local.holdFor(attrs, customer, false); h != nil {
			t.Fatal("an ordinary decision found the dead lease's hold")
		}
		if h, _ := g.local.holdFor(attrs, customer, true); h != warm {
			t.Fatal("safe spending lost the dead lease's hold")
		}
		f.set(func(f *fakeSync) { f.down = false })
		time.Sleep(limits.SyncInterval)
		synctest.Wait()
		ticket := mustAdmit(t, g, work(customer, "after", 1))
		if ticket.local == nil || ticket.local.hold == warm {
			t.Fatal("a request after the outage was decided from the dead lease's hold")
		}
		_ = ticket.Release(t.Context())
	})
}

func TestGateDominationDecidesWhoSharesARefusal(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	sent := leaseWant(work(customer, "sent", 10))
	for _, c := range []struct {
		p    billing.AdmitParams
		want bool
	}{
		{work(customer, "same", 10), true},
		{work(customer, "larger", 12), true},
		{work(customer, "smaller", 2), false},
	} {
		if got := dominates(c.p, &sent); got != c.want {
			t.Errorf("%s dominates = %v", c.p.RequestID, got)
		}
	}
	other := work(customer, "other-meta", 20)
	other.Usage.Metadata = map[string]any{"model": "large"}
	if dominates(other, &sent) {
		t.Error("other metadata may price in another cell")
	}
	stated := billing.AdmitParams{RequestID: "s", CustomerID: customer, Currency: "USD", EstimatedAmount: 500}
	w := leaseWant(stated)
	if w.EstimatedAmount == nil || *w.EstimatedAmount != 500 || w.Usage != nil {
		t.Fatalf("a host-stated want carries its estimate: %+v", w)
	}
	if !dominates(stated, &w) || dominates(billing.AdmitParams{EstimatedAmount: 400}, &w) || dominates(stated, &sent) {
		t.Fatal("host-stated estimates compare by amount, never against usage")
	}
	if attrsKey(work(customer, "a", 1)) == attrsKey(stated) {
		t.Fatal("requests of different event types share a hold key")
	}
}

func TestGateRollsOverWhenAnAnswerReportsTheLeaseDead(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 10 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 2)).Release(t.Context())
		synctest.Wait()
		first := leaseOf(g)
		f.set(func(f *fakeSync) { f.dead[first] = true })
		time.Sleep(1100 * time.Millisecond)
		if leaseOf(g) == first {
			t.Fatal("an answer that renewed nothing kept the lease")
		}
		var wants int
		var closed []billing.LeaseID
		f.set(func(f *fakeSync) { wants, closed = f.wants, slices.Clone(f.closed) })
		if !slices.Contains(closed, first) {
			t.Fatalf("closed %v: the lease answered dead is closed, so its holds free at once", closed)
		}
		mustAdmit(t, g, work(customer, "after", 1))
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.wants == wants {
			t.Fatal("the dead lease's hold still decided")
		}
	})
}

func TestGateTakesOverloadAsUnreachableAndKeepsUsage(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 20 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		synctest.Wait()
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			f.set(func(f *fakeSync) { f.reject = status })
			if _, err := g.Consume(t.Context(), work(customer, fmt.Sprint("warm-", status), 3)); err != nil {
				t.Fatalf("%d: a warm decision needs no answer: %v", status, err)
			}
			if _, err := g.Admit(t.Context(), work(billing.CustomerID(uuid.New()), "cold", 1)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%d: a cold request = %v, want ErrUnavailable", status, err)
			}
			if err := g.Flush(t.Context()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%d: flush = %v, want ErrUnavailable", status, err)
			}
		}
		f.set(func(f *fakeSync) { f.reject = http.StatusBadRequest })
		if err := g.Flush(t.Context()); err == nil || errors.Is(err, ErrUnavailable) || !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("flush refused by the call = %v, want its refusal", err)
		}
		f.set(func(f *fakeSync) { f.reject = 0 })
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 6 {
			t.Fatalf("settled %d, want the 6 units captured while syncs failed", got)
		}
	})
}

// stepOutbox is a memOutbox whose Put of one item waits and fails.
type stepOutbox struct {
	memOutbox
	failN   int64
	release chan struct{}
}

func (o *stepOutbox) Put(ctx context.Context, items []billing.OutboxItem) error {
	for _, it := range items {
		if it.Usage.N == o.failN && o.release != nil {
			<-o.release
			return errors.New("outbox: disk full")
		}
	}
	return o.memOutbox.Put(ctx, items)
}

func TestGateFailsAnUnstoredCaptureAndMovesUsagePastTheGap(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		outbox := &stepOutbox{memOutbox: memOutbox{items: map[billing.LeaseUsageRef]billing.LeaseUsage{}}, failN: 2, release: make(chan struct{})}
		g := newTestGate(t, f, GateDeps{Outbox: outbox})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		first := leaseOf(g)
		failed := make(chan error, 1)
		var lost *Ticket
		go func() {
			var err error
			lost, err = g.Consume(t.Context(), work(customer, "two", 2))
			failed <- err
		}()
		synctest.Wait()
		if _, err := g.Consume(t.Context(), work(customer, "three", 3)); err != nil {
			t.Fatal(err)
		}
		close(outbox.release)
		if err := <-failed; err == nil {
			t.Fatal("a capture whose Put failed succeeded")
		}
		if err := lost.Release(t.Context()); err != nil {
			t.Fatal("the failed capture left its ticket open:", err)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 4 {
			t.Fatalf("settled %d, want 1 + 3: the unstored capture never bills", got)
		}
		f.mu.Lock()
		through := f.leases[first].through
		f.mu.Unlock()
		if through != 1 || leaseOf(g) == first {
			t.Fatalf("the gapped lease settled through %d and stayed current: the item past the gap moves to a new lease", through)
		}
		outbox.mu.Lock()
		left := len(outbox.items)
		outbox.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d outbox items left: the moved item's old ref is acked and its new one settled", left)
		}
	})
}

func TestGateKeepsALiveLeaseItRolledOverUntilItsTicketsEnd(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		outbox := &stepOutbox{memOutbox: memOutbox{items: map[billing.LeaseUsageRef]billing.LeaseUsage{}}, failN: 2, release: make(chan struct{})}
		g := newTestGate(t, f, GateDeps{Outbox: outbox})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		// An open ticket, then a capture whose Put fails (item 2) and one
		// past the gap it leaves (item 3).
		open := mustAdmit(t, g, work(customer, "open", 5))
		first := leaseOf(g)
		failed := make(chan error, 1)
		var lost *Ticket
		go func() {
			var err error
			lost, err = g.Consume(t.Context(), work(customer, "lost", 2))
			failed <- err
		}()
		synctest.Wait()
		if _, err := g.Consume(t.Context(), work(customer, "past-the-gap", 3)); err != nil {
			t.Fatal(err)
		}
		close(outbox.release)
		if err := <-failed; err == nil {
			t.Fatal("a capture whose Put failed succeeded")
		}
		if err := lost.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		if leaseOf(g) == first {
			t.Fatal("the gapped lease stayed current")
		}
		time.Sleep(2 * limits.SyncInterval)
		f.mu.Lock()
		closed, held := slices.Contains(f.closed, first), f.holds[fakeHold{first, customer}]["quota"]
		f.mu.Unlock()
		if closed || held == 0 {
			t.Fatalf("closed %v, holding %d: a lease the server still holds is kept while a ticket of it is open", closed, held)
		}
		if err := open.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * limits.SyncInterval)
		f.mu.Lock()
		closed = slices.Contains(f.closed, first)
		f.mu.Unlock()
		if !closed {
			t.Fatal("the rolled-over lease was not closed once its last ticket ended")
		}
	})
}

func TestGateLeavesARolledOverLeaseToExpireWhileATicketOutlivesIt(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		open := mustAdmit(t, g, work(customer, "open", 5))
		flush(t, g)
		first := leaseOf(g)
		// The lease settled numbers the gate never issued: it rolls over,
		// and the server still holds it.
		f.set(func(f *fakeSync) { f.leases[first].through = 50 })
		time.Sleep(limits.SyncInterval + 100*time.Millisecond)
		if leaseOf(g) == first {
			t.Fatal("the lease was kept")
		}
		time.Sleep(limits.LeaseLifetime + limits.LeaseGrace + 2*limits.SyncInterval)
		if err := open.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * limits.SyncInterval)
		f.mu.Lock()
		defer f.mu.Unlock()
		if slices.Contains(f.closed, first) {
			t.Fatal("a lease whose ticket outlived its life was closed, not left to expire")
		}
		g.local.mu.Lock()
		left := len(g.local.closes)
		g.local.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d leases still waiting to close", left)
		}
	})
}

// leakyOutbox stores every item, but fails the first Put of number failN
// and every Ack of it: an outbox breaking Put's contract, whose failed Put
// stored the item anyway. The gate still never issues its number again.
type leakyOutbox struct {
	memOutbox
	failN int64
	kept  *billing.LeaseUsageRef
	puts  map[billing.LeaseUsageRef]int
}

func (o *leakyOutbox) Put(ctx context.Context, items []billing.OutboxItem) error {
	_ = o.memOutbox.Put(ctx, items)
	o.mu.Lock()
	defer o.mu.Unlock()
	var err error
	for _, it := range items {
		ref := billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}
		o.puts[ref]++
		if it.Usage.N == o.failN && o.kept == nil {
			o.kept, err = &ref, errors.New("outbox: stored, then the write timed out")
		}
	}
	return err
}

func (o *leakyOutbox) Ack(ctx context.Context, refs []billing.LeaseUsageRef) error {
	o.mu.Lock()
	kept := o.kept
	o.mu.Unlock()
	rest := slices.DeleteFunc(slices.Clone(refs), func(r billing.LeaseUsageRef) bool { return kept != nil && r == *kept })
	_ = o.memOutbox.Ack(ctx, rest)
	if len(rest) < len(refs) {
		return errors.New("outbox: ack timed out")
	}
	return nil
}

func TestGateNeverReusesTheNumberOfAFailedPut(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		outbox := &leakyOutbox{memOutbox: memOutbox{items: map[billing.LeaseUsageRef]billing.LeaseUsage{}}, failN: 2, puts: map[billing.LeaseUsageRef]int{}}
		g := newTestGate(t, f, GateDeps{Outbox: outbox})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		first := leaseOf(g)
		if _, err := g.Consume(t.Context(), work(customer, "two", 2)); err == nil {
			t.Fatal("a capture whose Put failed succeeded")
		}
		if _, err := g.Consume(t.Context(), work(customer, "three", 3)); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 4 {
			t.Fatalf("settled %d, want 1 + 3: the unstored capture never bills here", got)
		}
		if leaseOf(g) == first {
			t.Fatal("the gapped lease stayed current: the item past the gap moves to a new lease")
		}
		outbox.mu.Lock()
		defer outbox.mu.Unlock()
		for ref, n := range outbox.puts {
			if n > 1 {
				t.Fatalf("%v was put %d times: a failed Put's number is never issued again", ref, n)
			}
		}
		if _, ok := outbox.items[billing.LeaseUsageRef{LeaseID: first, N: 2}]; !ok {
			t.Fatal("the leaked item is still the outbox's only item under its number")
		}
	})
}

func TestGateResendsUsageASyncLeftUnsettled(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		f.set(func(f *fakeSync) { f.perSync, f.firstSeen = 2, nil })
		for i := range 5 {
			if _, err := g.Consume(t.Context(), work(customer, fmt.Sprint(i), 1)); err != nil {
				t.Fatal(err)
			}
		}
		flush(t, g)
		f.mu.Lock()
		seen := slices.Clone(f.firstSeen)
		f.mu.Unlock()
		if got := f.settledOf(customer, "quota"); got != 5 || !slices.Equal(seen, []int64{1, 2, 3, 4, 5}) {
			t.Fatalf("settled %d as %v, want 5 once each in order: items past through_n are sent again", got, seen)
		}
	})
}

func TestGateRollsOverWhenTheLeaseSettledNumbersItNeverIssued(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 50 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		first := leaseOf(g)
		f.set(func(f *fakeSync) { f.leases[first].through = 50 })
		time.Sleep(1100 * time.Millisecond)
		if leaseOf(g) == first {
			t.Fatal("a lease that settled numbers the gate never issued kept numbering")
		}
		if _, err := g.Consume(t.Context(), work(customer, "two", 2)); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 3 {
			t.Fatalf("settled %d, want 3", got)
		}
	})
}

func TestGateNeverMakesAnEmptyHold(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.noHolds = true
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		ticket, err := g.Admit(t.Context(), work(billing.CustomerID(uuid.New()), "no-hold", 1))
		if err != nil {
			t.Fatal(err)
		}
		g.local.index.RLock()
		holds := len(g.local.holds)
		g.local.index.RUnlock()
		if ticket.Allowed() || holds != 0 {
			t.Fatalf("admitted %v with %d local holds: a want answered without a hold admits nothing", ticket.Allowed(), holds)
		}
	})
}

func TestGateSmallerWaiterSendsItsOwnWant(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 5})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		f.set(func(f *fakeSync) { f.block = block })
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		type result struct {
			ticket *Ticket
			err    error
		}
		small, large := make(chan result, 1), make(chan result, 1)
		go func() {
			ticket, err := g.Admit(t.Context(), work(customer, "small", 2))
			small <- result{ticket, err}
		}()
		synctest.Wait()
		go func() {
			ticket, err := g.Admit(t.Context(), work(customer, "large", 10))
			large <- result{ticket, err}
		}()
		synctest.Wait()
		f.set(func(f *fakeSync) { f.block = nil })
		close(block)
		l, s := <-large, <-small
		if l.err != nil || l.ticket.Allowed() || l.ticket.Refusal().DenyCode != billing.DenyLimitReached {
			t.Fatalf("large = %+v, %v: refused limit_reached", l.ticket.Refusal(), l.err)
		}
		if s.err != nil || !s.ticket.Allowed() {
			t.Fatalf("small = %+v, %v: it fits, and does not inherit the larger want's refusal", s.ticket.Refusal(), s.err)
		}
	})
}

func TestGateReportsAppliedVersionsUntilASyncIsAnswered(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(cost int64) int64 { return 10 * cost }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		mustAdmit(t, g, work(customer, "grow", 20)).Release(t.Context())
		synctest.Wait()
		var version int64
		lease := leaseOf(g)
		f.set(func(f *fakeSync) {
			version = f.versions[fakeHold{lease, customer}]
			f.applied, f.dropReply = nil, 1
		})
		time.Sleep(3100 * time.Millisecond)
		f.mu.Lock()
		applied := slices.Clone(f.applied)
		f.mu.Unlock()
		want := []billing.HoldApplied{{CustomerID: customer, LimitSet: fakeSet, Version: version}}
		if len(applied) < 3 || !slices.Equal(applied[0], want) || !slices.Equal(applied[1], want) || len(applied[2]) != 0 {
			t.Fatalf("applied %+v, want %+v twice (its first sync's reply was lost), then none", applied, want)
		}
	})
}

func TestGateFlushIsBoundedWithoutADeadline(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		if _, err := g.Consume(t.Context(), work(customer, "one", 1)); err != nil {
			t.Fatal(err)
		}
		f.set(func(f *fakeSync) { f.stall = true })
		started := time.Now()
		if err := g.Flush(context.Background()); !errors.Is(err, ErrUnavailable) || time.Since(started) != gateFlushBound {
			t.Fatalf("flush that never settles = %v after %v, want ErrUnavailable after %v", err, time.Since(started), gateFlushBound)
		}
		f.set(func(f *fakeSync) { f.stall = false })
		flush(t, g)
	})
}

func TestGateClosesTheEarlierLeasesItResends(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 100})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		f.size = func(int64) int64 { return 20 }
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(customer, "warm-up", 1)).Release(t.Context())
		first := leaseOf(g)
		f.set(func(f *fakeSync) { f.down = true })
		if _, err := g.Consume(t.Context(), work(customer, "unsent", 2)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second)
		if leaseOf(g) == first {
			t.Fatal("a lease past its life by the local clock was kept")
		}
		f.set(func(f *fakeSync) { f.down = false })
		flush(t, g)
		f.mu.Lock()
		closed := slices.Clone(f.closed)
		through := f.leases[first].through
		f.mu.Unlock()
		if !slices.Contains(closed, first) || through != 1 {
			t.Fatalf("closed %v, old lease through %d: its usage is resent with close", closed, through)
		}
	})
}

func TestGateNeverReturnsAHoldWithAnOpenTicket(t *testing.T) {
	f := newFakeSync(map[string]int64{"quota": 10})
	f.pool(t, 3)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		g.local.mu.Lock()
		g.local.maxHolds = 2
		g.local.mu.Unlock()
		customers := []billing.CustomerID{billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())}
		open := mustAdmit(t, g, work(customers[0], "open", 1))
		time.Sleep(10 * time.Millisecond)
		for i, c := range customers[1:] {
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
		if len(returns) != 1 || returns[0].CustomerID != customers[1] {
			t.Fatalf("returns %+v, want the least recently used idle hold, not the one with an open ticket", returns)
		}
		if err := open.Capture(t.Context(), capture(1)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGateRecordKeyNamesCustomerEventTypeSourceAndSourceID(t *testing.T) {
	base := billing.LeaseUsage{CustomerID: billing.CustomerID(uuid.New()), EventType: "work", Source: "api", SourceID: "call-1", Dimensions: map[string]int64{"units": 1}}
	same := base
	same.Dimensions = map[string]int64{"units": 9}
	if recordKey(base) != recordKey(same) {
		t.Fatal("the same customer, event type, source and source_id are one Record")
	}
	for name, change := range map[string]func(*billing.LeaseUsage){
		"customer":   func(u *billing.LeaseUsage) { u.CustomerID = billing.CustomerID(uuid.New()) },
		"event type": func(u *billing.LeaseUsage) { u.EventType = "other" },
		"source":     func(u *billing.LeaseUsage) { u.Source = "batch" },
		"source_id":  func(u *billing.LeaseUsage) { u.SourceID = "call-2" },
	} {
		other := base
		change(&other)
		if recordKey(other) == recordKey(base) {
			t.Fatalf("another %s is another Record", name)
		}
	}
}

func TestGateReclaimsTicketsOfDeadProcessesAndXTicketsPastTheirLease(t *testing.T) {
	dead := map[string]bool{"gone": true}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC).UnixMilli()
	ttl := gateEarlierTicketTTL.Milliseconds()
	made := func(ago int64) string { return strconv.FormatInt(now-ago, 10) }
	for _, c := range []struct {
		ticket string
		want   bool
	}{
		{"o|gone|quota@=3", true},
		{"o|live|quota@=3", false},
		{"c|7|quota@=3", false},
		{"x|gone|lse_old|" + made(0) + "|quota@=3", true},
		{"x|live|lse_old|" + made(ttl-1) + "|quota@=3", false},
		{"x|live|lse_old|" + made(ttl) + "|quota@=3", true},
		// A captured ticket whose usage was lost with its lease's stream.
		{"x|7|lse_old|" + made(ttl+time.Hour.Milliseconds()) + "|quota@=3", true},
		{"x|7|lse_old|" + made(0) + "|quota@=3", false},
		{"x|7|lse_old|quota@=3", false},
	} {
		if got := reclaimable(c.ticket, dead, now); got != c.want {
			t.Errorf("reclaimable(%q) = %v, want %v", c.ticket, got, c.want)
		}
	}
}
