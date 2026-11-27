package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
)

// fakeViews speaks the view protocol: limits count the event's "units";
// with money, balance costs 1000 micros a unit. Usage settles in order, and
// each sync replaces its lease's in-flight report.
type fakeViews struct {
	mu       sync.Mutex
	merchant billing.MerchantID
	limits   map[string]fakeLimit
	money    bool
	balance  int64
	settled  map[fakeKey]int64
	inFlight map[billing.LeaseID]map[fakeKey]int64
	leases   map[billing.LeaseID]*fakeLease
	down     bool
	drop     int
	perSync  int
	syncs    []billing.SyncLeasesParams
	clients  []*Client
	// broken customers' views are refused (as a policy that fails to
	// resolve), deferred ones answered null; settleNone settles no new usage;
	// slow delays answers.
	broken     map[billing.CustomerID]bool
	deferred   map[billing.CustomerID]bool
	settleNone bool
	slow       time.Duration
	// busyUntil answers every sync before it 503, as a server that could
	// not take its locks in time.
	busyUntil time.Time
	// refuse answers the next sync with that status.
	refuse int
	// hold keeps the next request in flight until it closes; held closes
	// once it is held (holdNext).
	hold, held chan struct{}
}

type fakeLimit struct {
	capacity int64
	window   time.Duration
	overrun  catalog.Overrun
}

type fakeKey struct {
	customer billing.CustomerID
	limit    string
	window   int64
}

type fakeLease struct {
	through int64
	items   map[int64]string
	last    time.Time
	closed  bool
}

func newFakeViews(limits map[string]fakeLimit) *fakeViews {
	return &fakeViews{merchant: billing.MerchantID(uuid.New()), limits: limits, settled: map[fakeKey]int64{},
		inFlight: map[billing.LeaseID]map[fakeKey]int64{}, leases: map[billing.LeaseID]*fakeLease{}}
}

func (f *fakeViews) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	down, slow, hold, held := f.down, f.slow, f.hold, f.held
	f.hold, f.held = nil, nil
	f.mu.Unlock()
	if hold != nil {
		close(held)
		select {
		case <-hold:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	if down {
		return nil, errors.New("fake: connection refused")
	}
	rec := httptest.NewRecorder()
	if slow == 0 {
		f.ServeHTTP(rec, r)
		return rec.Result(), nil
	}
	// The server answers after slow, whether or not the caller still waits.
	done := make(chan struct{})
	go func() {
		time.Sleep(slow)
		f.ServeHTTP(rec, r.WithContext(context.Background()))
		close(done)
	}()
	select {
	case <-done:
		return rec.Result(), nil
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

// pool makes n clients outside a synctest bubble: a client starts its read
// cache's goroutines when made.
func (f *fakeViews) pool(t *testing.T, n int) {
	for range n {
		remote, err := NewRemote("https://openrails.test", RemoteOptions{Credentials: map[billing.MerchantID]RemoteCredential{f.merchant: {APIKey: "test-key"}}, HTTPClient: &http.Client{Transport: f}})
		if err != nil {
			t.Fatal(err)
		}
		c, err := remote.Merchant(f.merchant)
		if err != nil {
			t.Fatal(err)
		}
		f.clients = append(f.clients, c)
	}
}

func (f *fakeViews) keys() []string {
	keys := []string{}
	if f.money {
		keys = append(keys, catalog.BalanceLimit)
	}
	for k := range f.limits {
		keys = append(keys, k)
	}
	slices.Sort(keys[min(len(keys), boolInt(f.money)):])
	return keys
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *fakeViews) cost(limit string, units int64) int64 {
	if limit == catalog.BalanceLimit {
		return units * 1000
	}
	return units
}

func (f *fakeViews) window(limit string, at time.Time) (int64, *time.Time, *time.Time) {
	w := f.limits[limit].window
	if w == 0 {
		return 0, nil, nil
	}
	start := at.UTC().Truncate(w)
	end := start.Add(w)
	return start.UnixMicro(), &start, &end
}

// usedLocked is what is settled, held and reported in flight of a limit's
// current window, other than except's report.
func (f *fakeViews) usedLocked(customer billing.CustomerID, limit string, now time.Time, except billing.LeaseID) (settled, held int64) {
	win, _, _ := f.window(limit, now)
	settled = f.settled[fakeKey{customer, limit, win}]
	for lease, lines := range f.inFlight {
		if lease != except && f.live(lease, now) {
			held += lines[fakeKey{customer, limit, 0}]
		}
	}
	return settled, held
}

func (f *fakeViews) live(lease billing.LeaseID, now time.Time) bool {
	l := f.leases[lease]
	return l != nil && !l.closed && now.Sub(l.last) < 10*time.Second
}

func (f *fakeViews) capacity(limit string) int64 {
	if limit == catalog.BalanceLimit {
		return f.balance
	}
	return f.limits[limit].capacity
}

func (f *fakeViews) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(merchant.BoundHeader, f.merchant.String())
	switch r.URL.Path {
	case "/v1/app/leases/sync":
		f.sync(w, r)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeViews) sync(w http.ResponseWriter, r *http.Request) {
	var p billing.SyncLeasesParams
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeFakeError(w, 400, billing.CodeInvalidParam, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if now.Before(f.busyUntil) {
		writeFakeError(w, 503, billing.CodeServiceUnavailable, "the sync could not take its locks in time; retry it")
		return
	}
	if status := f.refuse; status != 0 {
		f.refuse = 0
		writeFakeError(w, status, billing.CodeInvalidParam, "refused")
		return
	}
	f.syncs = append(f.syncs, p)
	lease := f.leases[p.LeaseID]
	if lease == nil {
		lease = &fakeLease{items: map[int64]string{}}
		f.leases[p.LeaseID] = lease
	}
	if !p.Close && !lease.closed {
		lease.last = now
	}
	res := billing.SyncLeasesResult{Lease: billing.Lease{ID: p.LeaseID, ExpiresAt: now.Add(10 * time.Second).UTC()},
		Usage: billing.LeaseUsageResult{Refused: []billing.LeaseUsageRefusal{}}, Views: []billing.View{}, LimitSets: []*billing.LimitSet{}, Pricing: []billing.ViewPricing{}}
	fresh := 0
	for i, u := range p.Usage {
		if (f.settleNone || f.perSync > 0 && fresh == f.perSync) && u.N > lease.through {
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
		at := now
		if u.AdmittedAt != nil {
			at = *u.AdmittedAt
		}
		for _, k := range f.keys() {
			win, _, _ := f.window(k, at)
			c := f.cost(k, u.Dimensions["units"])
			f.settled[fakeKey{u.CustomerID, k, win}] += c
		}
	}
	res.Usage.ThroughN = lease.through
	if p.Close || lease.closed {
		lease.closed = true
		delete(f.inFlight, p.LeaseID)
	} else {
		lines := map[fakeKey]int64{}
		for _, l := range p.InFlight {
			lines[fakeKey{l.CustomerID, l.Limit, 0}] += l.Amount
		}
		f.inFlight[p.LeaseID] = lines
	}
	viewed := map[billing.CustomerID]bool{}
	priced := map[string]bool{}
	for i, vp := range p.Views {
		switch {
		case f.broken[vp.CustomerID]:
			res.LimitSets = append(res.LimitSets, nil)
			res.RefusedViews = append(res.RefusedViews, billing.ViewRefusal{Index: i, Status: 500, Error: &billing.ErrorDetails{Type: billing.ErrorTypeAPI, Code: billing.CodeInternalError}})
			continue
		case vp.InvokerType != billing.InvokerTypeCustomer && vp.InvokerType != billing.InvokerTypeDelegated:
			res.LimitSets = append(res.LimitSets, nil)
			res.RefusedViews = append(res.RefusedViews, billing.ViewRefusal{Index: i, Status: 400, Error: &billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeInvalidParam}})
			continue
		case f.deferred[vp.CustomerID]:
			res.LimitSets = append(res.LimitSets, nil)
			continue
		}
		set := &billing.LimitSet{CustomerID: vp.CustomerID, Currency: vp.Currency, Limits: f.keys(), BalanceDenyCode: billing.DenyInsufficientBalance}
		res.LimitSets = append(res.LimitSets, set)
		if !viewed[vp.CustomerID] {
			viewed[vp.CustomerID] = true
			res.Views = append(res.Views, f.view(vp.CustomerID, now, p.LeaseID))
		}
		if vp.EventType != nil && !priced[*vp.EventType] {
			priced[*vp.EventType] = true
			res.Pricing = append(res.Pricing, f.pricing(*vp.EventType))
		}
	}
	for id, l := range f.leases {
		if f.live(id, now) || id == p.LeaseID && !l.closed {
			res.Gates++
		}
	}
	if f.drop > 0 {
		f.drop--
		writeFakeError(w, 503, "unavailable", "reply lost")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (f *fakeViews) view(customer billing.CustomerID, now time.Time, except billing.LeaseID) billing.View {
	v := billing.View{CustomerID: customer, Currency: "USD", AsOf: now.UTC(), Limits: []billing.ViewLimit{}}
	for _, k := range f.keys() {
		settled, held := f.usedLocked(customer, k, now, except)
		unit := "units"
		if k == catalog.BalanceLimit {
			unit = "USD"
		}
		line := billing.ViewLimit{Limit: k, Unit: unit, Overrun: catalog.OverrunRequireEstimate, Capacity: f.capacity(k), Used: settled, Held: held,
			Remaining: f.capacity(k) - settled - held}
		if def, ok := f.limits[k]; ok {
			if def.overrun != "" {
				line.Overrun = def.overrun
			}
			reset := catalog.LimitReset{Kind: catalog.ResetLifetime}
			if def.window > 0 {
				reset = catalog.LimitReset{Kind: catalog.ResetFixed, WindowSeconds: int(def.window / time.Second)}
				_, start, end := f.window(k, now)
				nextEnd := end.Add(def.window)
				line.WindowStartsAt, line.WindowEndsAt = start, end
				line.Next = &billing.ViewWindow{StartsAt: end, EndsAt: &nextEnd, Remaining: def.capacity}
			}
			line.Reset = &reset
		}
		v.Limits = append(v.Limits, line)
	}
	return v
}

// pricing prices "work"; every other event type has no card or count.
func (f *fakeViews) pricing(eventType string) billing.ViewPricing {
	p := billing.ViewPricing{Currency: "USD", EventType: eventType, Counts: []billing.ViewCount{}, RateCards: []billing.ViewRateCard{}, PriceBounds: []billing.PriceBound{}}
	if eventType != "work" {
		return p
	}
	for k := range f.limits {
		p.Counts = append(p.Counts, billing.ViewCount{Limit: k, Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum, Weight: 1, Per: 1})
	}
	if f.money {
		p.RateCards = append(p.RateCards, billing.ViewRateCard{Meter: "work", EventType: "work", ValueProperty: "units", Aggregation: catalog.AggregationSum,
			Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1000, DivideBy: 1}}})
		p.PriceBounds = append(p.PriceBounds, billing.PriceBound{Meter: "work", UnitAmount: 1000, DivideBy: 1})
	}
	return p
}

func writeFakeError(w http.ResponseWriter, status int, code billing.ErrorCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": billing.ErrorDetails{Type: billing.ErrorTypeAPI, Code: code, Message: msg}})
}

func (f *fakeViews) set(fn func(f *fakeViews)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// holdNext keeps the next request in flight until release closes: it fails
// as unreachable if OpenRails was when it was sent. The returned channel
// closes once it is held.
func (f *fakeViews) holdNext(release chan struct{}) <-chan struct{} {
	held := make(chan struct{})
	f.set(func(f *fakeViews) { f.hold, f.held = release, held })
	return held
}

func (f *fakeViews) settledOf(customer billing.CustomerID, limit string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for k, v := range f.settled {
		if k.customer == customer && k.limit == limit {
			total += v
		}
	}
	return total
}

func work(customer billing.CustomerID, id string, units int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": units}}}
}

func capture(units int64) billing.CaptureUsage {
	return billing.CaptureUsage{Dimensions: map[string]int64{"units": units}}
}

func newTestGate(t *testing.T, f *fakeViews, deps GateDeps) *Gate {
	t.Helper()
	f.mu.Lock()
	if len(f.clients) == 0 {
		f.mu.Unlock()
		t.Fatal("no client left: create them outside the bubble with pool")
	}
	c := f.clients[0]
	f.clients = f.clients[1:]
	f.mu.Unlock()
	g, err := NewGate(c, deps)
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

// warm admits and captures one unit so the gate holds the customer's view.
func warm(t *testing.T, g *Gate, customer billing.CustomerID) {
	t.Helper()
	tk := mustAdmit(t, g, work(customer, "warm-"+uuid.NewString(), 1))
	if err := tk.Capture(t.Context(), capture(1)); err != nil {
		t.Fatal(err)
	}
	flush(t, g)
}

func TestGateRacesOnOneViewNeverTakePastEitherLimit(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"hourly": {capacity: 100}, "daily": {capacity: 60}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		var mu sync.Mutex
		admitted := int64(0)
		var wg sync.WaitGroup
		for i := range 80 {
			wg.Go(func() {
				tk, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("r-%d", i), 1))
				if err != nil || !tk.Allowed() {
					return
				}
				mu.Lock()
				admitted++
				mu.Unlock()
			})
		}
		wg.Wait()
		// daily has 59 left, and one gate decides against its view less what
		// it has open: the limit is never passed.
		if admitted != 59 {
			t.Fatalf("admitted %d past daily's 59", admitted)
		}
	})
}

func TestGateCapturesInPlaceOfItsTake(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		tk := mustAdmit(t, g, work(customer, "big", 40))
		lines, err := g.ListCustomerLimits(t.Context(), customer)
		if err != nil {
			t.Fatal(err)
		}
		if lines[0].Remaining != 59 {
			t.Fatalf("remaining with 40 open = %d, want 59", lines[0].Remaining)
		}
		if err := tk.Capture(t.Context(), capture(30)); err != nil {
			t.Fatal(err)
		}
		lines, _ = g.ListCustomerLimits(t.Context(), customer)
		if lines[0].Remaining != 69 {
			t.Fatalf("remaining after capturing 30 = %d, want 69", lines[0].Remaining)
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 31 {
			t.Fatalf("settled %d, want 31", got)
		}
	})
}

func TestGateSettlesOnceAfterALostReply(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		tk := mustAdmit(t, g, work(customer, "a", 5))
		f.set(func(f *fakeViews) { f.drop = 1 })
		if err := tk.Capture(t.Context(), capture(5)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got != 6 {
			t.Fatalf("settled %d after a lost reply, want 6", got)
		}
		lines, _ := g.ListCustomerLimits(t.Context(), customer)
		if lines[0].Remaining != 994 {
			t.Fatalf("remaining %d, want 994: settled usage counted once", lines[0].Remaining)
		}
	})
}

func TestGateReportsOnlyTicketsOpenAcrossASync(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		short := mustAdmit(t, g, work(customer, "short", 3))
		long := mustAdmit(t, g, work(customer, "long", 7))
		if err := short.Capture(t.Context(), capture(3)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3500 * time.Millisecond)
		f.mu.Lock()
		var reported int64
		for _, l := range f.inFlight[g.g.lease] {
			reported += l
		}
		f.mu.Unlock()
		if reported != 7 {
			t.Fatalf("reported %d in flight, want the long ticket's 7 only", reported)
		}
		if err := long.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		f.mu.Lock()
		reported = 0
		for _, l := range f.inFlight[g.g.lease] {
			reported += l
		}
		f.mu.Unlock()
		if reported != 0 {
			t.Fatalf("reported %d after release, want 0", reported)
		}
	})
}

func TestGateSpendsItsShareWhileUnreachable(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 2)
	synctest.Test(t, func(t *testing.T) {
		a, b := newTestGate(t, f, GateDeps{}), newTestGate(t, f, GateDeps{})
		defer closeGate(t, a)
		defer closeGate(t, b)
		customer := billing.CustomerID(uuid.New())
		warm(t, a, customer)
		warm(t, b, customer)
		// Both gates sync while the other is live, as active gates do.
		for _, g := range []*Gate{a, b} {
			mustAdmit(t, g, work(customer, fmt.Sprintf("live-%p", g), 1)).Release(t.Context())
		}
		time.Sleep(1500 * time.Millisecond)
		f.set(func(f *fakeViews) { f.down = true })
		// Each gate learns OpenRails is unreachable at its next sync.
		for _, g := range []*Gate{a, b} {
			if _, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("first-%p", g), 1)); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(1500 * time.Millisecond)
		var total int64 = 2
		for _, g := range []*Gate{a, b} {
			for i := range 100 {
				tk, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("off-%p-%d", g, i), 1))
				if err != nil {
					if !errors.Is(err, ErrUnavailable) {
						t.Fatal(err)
					}
					break
				}
				if tk.Allowed() {
					total++
				}
			}
		}
		if total > 98 {
			t.Fatalf("two gates spent %d offline of the 98 left", total)
		}
		if a.g.gates != 2 || b.g.gates != 2 {
			t.Fatalf("gates counted %d and %d, want 2", a.g.gates, b.g.gates)
		}
		if total < 40 {
			t.Fatalf("two gates spent only %d offline", total)
		}
	})
}

func TestGateDecidesOneRequestOnAStaleView(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		time.Sleep(30 * time.Second)
		f.mu.Lock()
		quiet := len(f.syncs)
		f.mu.Unlock()
		time.Sleep(30 * time.Second)
		f.mu.Lock()
		idle := len(f.syncs) - quiet
		f.mu.Unlock()
		if idle != 0 {
			t.Fatalf("an idle gate synced %d times", idle)
		}
		// Answers take a second, so a sync the probe starts cannot land
		// before it is counted.
		f.set(func(f *fakeViews) { f.slow = time.Second })
		mustAdmit(t, g, work(customer, "probe", 1))
		f.mu.Lock()
		after := len(f.syncs)
		f.mu.Unlock()
		if after != quiet {
			t.Fatalf("the probe waited for a sync")
		}
		mustAdmit(t, g, work(customer, "next", 1))
		f.mu.Lock()
		waited := len(f.syncs) - after
		f.mu.Unlock()
		if waited == 0 {
			t.Fatalf("the second request on a stale view decided without a sync")
		}
	})
}

func TestGateMovesIntoTheNextWindowWithoutASync(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"minute": {capacity: 5, window: time.Minute}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		time.Sleep(55 * time.Second)
		f.set(func(f *fakeViews) { f.down = true })
		time.Sleep(6 * time.Second)
		f.set(func(f *fakeViews) { f.down = false })
		tk, err := g.Admit(t.Context(), work(customer, "next-window", 1))
		if err != nil || !tk.Allowed() {
			t.Fatalf("the next window's first request: %v %+v", err, tk.Refusal())
		}
	})
}

type memOutbox struct {
	mu    sync.Mutex
	items map[string]billing.OutboxItem
	fail  int
}

func (o *memOutbox) Put(_ context.Context, items []billing.OutboxItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail > 0 {
		o.fail--
		return errors.New("outbox down")
	}
	for _, it := range items {
		o.items[fmt.Sprintf("%s/%d", it.LeaseID, it.Usage.N)] = it
	}
	return nil
}

func (o *memOutbox) Pending(context.Context) ([]billing.OutboxItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := []billing.OutboxItem{}
	for _, it := range o.items {
		out = append(out, it)
	}
	return out, nil
}

func (o *memOutbox) Ack(_ context.Context, refs []billing.LeaseUsageRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range refs {
		delete(o.items, fmt.Sprintf("%s/%d", r.LeaseID, r.N))
	}
	return nil
}

func TestGateOutboxNeverSkipsANumberAndResendsAfterRestart(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 2)
	synctest.Test(t, func(t *testing.T) {
		box := &memOutbox{items: map[string]billing.OutboxItem{}}
		g := newTestGate(t, f, GateDeps{Outbox: box})
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		box.mu.Lock()
		box.fail = 1
		box.mu.Unlock()
		failed := mustAdmit(t, g, work(customer, "failed-put", 2))
		if err := failed.Capture(t.Context(), capture(2)); err == nil {
			t.Fatal("a capture whose Put failed succeeded")
		}
		if err := failed.Capture(t.Context(), capture(2)); err != nil {
			t.Fatal(err)
		}
		f.set(func(f *fakeViews) { f.down = true })
		later := mustAdmit(t, g, work(customer, "unsent", 3))
		if err := later.Capture(t.Context(), capture(3)); err != nil {
			t.Fatal(err)
		}
		// The process dies with usage unsent; a new one resends it.
		g.g.mu.Lock()
		g.g.stopped = true
		g.g.mu.Unlock()
		close(g.g.stop)
		<-g.g.done
		f.set(func(f *fakeViews) { f.down = false })
		restarted := newTestGate(t, f, GateDeps{Outbox: box})
		defer closeGate(t, restarted)
		warm(t, restarted, customer)
		time.Sleep(3 * time.Second)
		flush(t, restarted)
		if got := f.settledOf(customer, "quota"); got != 1+2+3+1 {
			t.Fatalf("settled %d, want 7: each item once", got)
		}
		f.mu.Lock()
		closed := f.leases[g.g.lease].closed
		f.mu.Unlock()
		if !closed {
			t.Fatal("the dead process's lease was not closed by its resend")
		}
	})
}

func TestGateRefusesBadInput(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 10}})
	f.pool(t, 1)
	g := newTestGate(t, f, GateDeps{})
	defer closeGate(t, g)
	customer := billing.CustomerID(uuid.New())
	for name, p := range map[string]billing.AdmitParams{
		"no request id":  {CustomerID: customer, Currency: "USD"},
		"no customer":    {RequestID: "r", Currency: "USD"},
		"no currency":    {RequestID: "r", CustomerID: customer},
		"usage and cost": {RequestID: "r", CustomerID: customer, Currency: "USD", EstimatedAmount: 1, Usage: &billing.UsageEstimate{EventType: "work"}},
	} {
		if _, err := g.Admit(t.Context(), p); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	if _, err := NewGate(nil, GateDeps{}); err == nil {
		t.Error("a gate with no handle")
	}
}

func hasCode(err error, code billing.ErrorCode) bool {
	got, ok := billing.ErrorCodeFrom(err)
	return ok && got == code
}

func reportedOf(f *fakeViews, lease billing.LeaseID) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, l := range f.inFlight[lease] {
		total += l
	}
	return total
}

func syncCount(f *fakeViews) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncs)
}

func TestGateRefusesARequestItCannotViewAlone(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		ok, broken := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		f.set(func(f *fakeViews) { f.broken = map[billing.CustomerID]bool{broken: true} })
		warm(t, g, ok)
		// No sync warming started is counted below.
		synctest.Wait()
		// What no server would take is refused before anything is sent.
		bad := work(ok, "btc", 1)
		bad.Currency = "BTC"
		if _, err := g.Admit(t.Context(), bad); !hasCode(err, billing.CodeCurrencyUnsupported) {
			t.Fatalf("an unsupported currency: %v", err)
		}
		bad = work(ok, "agent", 1)
		bad.InvokerType = "agent"
		if _, err := g.Admit(t.Context(), bad); !hasCode(err, billing.CodeInvalidParam) {
			t.Fatalf("an unknown invoker type: %v", err)
		}
		// A view the server refuses answers its request alone, once.
		before := syncCount(f)
		if _, err := g.Admit(t.Context(), work(broken, "broken", 1)); !hasCode(err, billing.CodeInternalError) {
			t.Fatalf("a refused view: %v", err)
		}
		if n := syncCount(f) - before; n != 1 {
			t.Fatalf("a refused view took %d syncs, want 1", n)
		}
		// The rest of the gate goes on: usage settles and the gate goes idle.
		warm(t, g, ok)
		if got := f.settledOf(ok, "quota"); got != 2 {
			t.Fatalf("settled %d, want 2", got)
		}
		time.Sleep(5 * time.Second)
		quiet := syncCount(f)
		time.Sleep(30 * time.Second)
		if n := syncCount(f) - quiet; n != 0 {
			t.Fatalf("the gate synced %d more times for a refused view", n)
		}
	})
}

// A sync can answer a view between its request asking for it and waiting
// for it: the answer, a refusal too, is the request's, and the sync puts
// the views first.
func TestGateKeepsAnAnswerThatComesBeforeItsRequestWaits(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		s := g.g
		busy, broken, cold := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		f.set(func(f *fakeViews) { f.broken = map[billing.CustomerID]bool{broken: true} })
		warm(t, g, busy)
		synctest.Wait()
		before := syncCount(f)
		ask := func(p billing.AdmitParams) (billing.AdmitParams, *viewWait) {
			if err := normalizeGateAdmit(&p); err != nil {
				t.Fatal(err)
			}
			return p, s.serve(attrsKey(p), p)
		}
		pb, wb := ask(work(broken, "broken", 1))
		pc, wc := ask(work(cold, "cold", 1))
		// Queued usage starts the sync that answers both before either waits.
		if err := mustAdmit(t, g, work(busy, "queued", 1)).Capture(t.Context(), capture(1)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		answered := syncCount(f)
		if err := s.await(t.Context(), attrsKey(pb), pb, wb); !hasCode(err, billing.CodeInternalError) {
			t.Fatalf("a view refused before its request waited: %v", err)
		}
		if err := s.await(t.Context(), attrsKey(pc), pc, wc); err != nil {
			t.Fatalf("a view answered before its request waited: %v", err)
		}
		synctest.Wait()
		if n := syncCount(f) - answered; n != 0 {
			t.Errorf("waiting for views a sync had answered took %d more syncs", n)
		}
		f.mu.Lock()
		syncs := slices.Clone(f.syncs[before:answered])
		f.mu.Unlock()
		first := slices.IndexFunc(syncs, func(p billing.SyncLeasesParams) bool {
			return slices.ContainsFunc(p.Views, func(v billing.ViewParams) bool { return v.CustomerID == broken })
		})
		if first < 0 {
			t.Fatal("no sync asked for the refused view")
		}
		if n := len(syncs[first].Usage); n > 0 {
			t.Errorf("the sync answering views requests were about to wait for carried %d usage items", n)
		}
		if got := f.settledOf(busy, "quota"); got != 2 {
			t.Errorf("settled %d, want 2", got)
		}
		// The busy customer's wait was answered with no request on it.
		s.mu.Lock()
		left := len(s.waiters)
		s.mu.Unlock()
		if left != 0 {
			t.Errorf("%d answered waits left", left)
		}
	})
}

func TestGateGivesUpOnAViewNoSyncAnswers(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		f.set(func(f *fakeViews) { f.deferred = map[billing.CustomerID]bool{customer: true} })
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err := g.Admit(ctx, work(customer, "deferred", 1))
		if !errors.Is(err, ErrUnavailable) || ctx.Err() != nil {
			t.Fatalf("a view every sync defers: %v (ctx %v)", err, ctx.Err())
		}
		// The deferred view is asked again each interval, not back to back.
		before := syncCount(f)
		time.Sleep(3 * time.Second)
		if n := syncCount(f) - before; n > 4 {
			t.Fatalf("the gate synced %d times in three intervals for a deferred view", n)
		}
	})
}

func TestGateKeepsAViewWhileItHasWorkInFlight(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 100}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		long := mustAdmit(t, g, work(customer, "long", 30))
		// Longer than an unused view is kept; the long ticket is still open.
		time.Sleep(viewKeep + time.Minute)
		var admitted int64
		for i := range 20 {
			tk, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("after-%d", i), 10))
			if err != nil {
				t.Fatal(err)
			}
			if tk.Allowed() {
				admitted += 10
			}
		}
		if admitted > 69 {
			t.Fatalf("admitted %d beside the long ticket's 30, of 99 left", admitted)
		}
		if err := long.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// The sync that carries usage settles it, so no view counts it twice: a
// captured ticket the last sync reported leaves the report in the sync that
// carries its usage, and so does usage a lost reply makes the gate resend.
func TestGateNeverReportsUsageItCarries(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		carriedReported := func(id string) {
			t.Helper()
			f.mu.Lock()
			defer f.mu.Unlock()
			carried := 0
			for _, p := range f.syncs {
				for _, u := range p.Usage {
					if u.SourceID != id {
						continue
					}
					carried++
					for _, l := range p.InFlight {
						if l.Limit == "quota" && l.Amount > 0 {
							t.Fatalf("a sync carrying %s also reported %d of quota in flight", id, l.Amount)
						}
					}
				}
			}
			if carried == 0 {
				t.Fatalf("no sync carried %s", id)
			}
		}

		long := mustAdmit(t, g, work(customer, "long", 7))
		time.Sleep(3500 * time.Millisecond)
		if got := reportedOf(f, g.g.lease); got != 7 {
			t.Fatalf("reported %d, want the open ticket's 7", got)
		}
		if err := long.Capture(t.Context(), capture(7)); err != nil {
			t.Fatal(err)
		}
		flush(t, g)
		carriedReported("long")
		if got := reportedOf(f, g.g.lease); got != 0 {
			t.Fatalf("reported %d once carried, want 0", got)
		}

		// A lost reply: the resend carries the usage again, reported nowhere.
		f.set(func(f *fakeViews) { f.drop = 1 })
		resent := mustAdmit(t, g, work(customer, "resent", 5))
		time.Sleep(3500 * time.Millisecond)
		if err := resent.Capture(t.Context(), capture(5)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		flush(t, g)
		carriedReported("resent")
		if got := f.settledOf(customer, "quota"); got != 13 {
			t.Fatalf("settled %d, want 13: each item once", got)
		}
	})
}

func TestGateDecidesAndCapturesConcurrentlyWithSyncs(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1_000_000}})
	f.pool(t, 1)
	g := newTestGate(t, f, GateDeps{})
	defer closeGate(t, g)
	customer := billing.CustomerID(uuid.New())
	warm(t, g, customer)
	var wg sync.WaitGroup
	deadline := time.Now().Add(1500 * time.Millisecond)
	for w := range 8 {
		wg.Go(func() {
			for i := 0; time.Now().Before(deadline); i++ {
				tk, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("c-%d-%d", w, i), 1))
				if err != nil || !tk.Allowed() {
					continue
				}
				_ = tk.Capture(t.Context(), capture(1))
			}
		})
	}
	wg.Wait()
}

func TestGateCaptureRacingCloseAnswers(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		box := &memOutbox{items: map[string]billing.OutboxItem{}}
		g := newTestGate(t, f, GateDeps{Outbox: box})
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		tk := mustAdmit(t, g, work(customer, "late", 1))
		closeGate(t, g)
		// A capture that passed the closed check as Close stopped the writer.
		g.g.mu.Lock()
		g.g.stopped = false
		g.g.mu.Unlock()
		if err := g.g.capture(t.Context(), tk, capture(1)); !errors.Is(err, errGateClosed) {
			t.Fatalf("a capture racing Close: %v", err)
		}
	})
}

func TestGateCloseEndsItsLease(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 10)
	synctest.Test(t, func(t *testing.T) {
		customer := billing.CustomerID(uuid.New())
		for i := range 10 {
			f.set(func(f *fakeViews) { f.slow = 0 })
			g := newTestGate(t, f, GateDeps{})
			warm(t, g, customer)
			tk := mustAdmit(t, g, work(customer, fmt.Sprintf("open-%d", i), 1))
			// A sync built before Close is still in flight when it begins.
			f.set(func(f *fakeViews) { f.slow = 400 * time.Millisecond })
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 100*time.Millisecond)))
			_ = tk.Release(t.Context())
			closeGate(t, g)
			f.mu.Lock()
			closed := f.leases[g.g.lease].closed
			f.mu.Unlock()
			if !closed {
				t.Fatalf("round %d: Close returned with its lease open", i)
			}
		}
	})
}

// A transient failure never drops captured usage: Close retries within its
// bound and settles it. Usage still unsent when the bound ends is named in
// Close's error.
func TestGateCloseRidesOutTransientFailures(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 2)
	synctest.Test(t, func(t *testing.T) {
		customer := billing.CustomerID(uuid.New())
		use := func(g *Gate, prefix string, n int) {
			for i := range n {
				tk := mustAdmit(t, g, work(customer, fmt.Sprintf("%s-%d", prefix, i), 2))
				if err := tk.Capture(t.Context(), capture(2)); err != nil {
					t.Fatal(err)
				}
			}
		}
		g := newTestGate(t, f, GateDeps{})
		warm(t, g, customer)
		f.set(func(f *fakeViews) { f.busyUntil = time.Now().Add(3 * time.Second) })
		use(g, "brief", 5)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := g.Close(ctx); err != nil {
			t.Fatalf("Close through a brief outage: %v", err)
		}
		if got := f.settledOf(customer, "quota"); got != 11 {
			t.Fatalf("%d units settled, want 11: Close dropped captured usage", got)
		}

		g = newTestGate(t, f, GateDeps{})
		warm(t, g, customer)
		f.set(func(f *fakeViews) { f.busyUntil = time.Now().Add(time.Hour) })
		use(g, "long", 2)
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := g.Close(ctx)
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "2 usage items unsent") {
			t.Fatalf("Close past its bound: %v, want ErrUnavailable naming 2 unsent items", err)
		}
	})
}

// A sync sent while OpenRails was unreachable can fail after it is back and
// a Flush has begun: that failure is not the Flush's.
func TestGateFlushCountsOnlySyncsSentAfterItBegins(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		sent, next := make(chan struct{}), make(chan struct{})
		f.set(func(f *fakeViews) { f.down = true })
		held := f.holdNext(sent)
		tk := mustAdmit(t, g, work(customer, "outage", 2))
		if err := tk.Capture(t.Context(), capture(2)); err != nil {
			t.Fatal(err)
		}
		<-held
		f.set(func(f *fakeViews) { f.down = false })
		flushed := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			flushed <- g.Flush(ctx)
		}()
		synctest.Wait()
		// The held sync fails while the next is in flight.
		held = f.holdNext(next)
		close(sent)
		<-held
		synctest.Wait()
		close(next)
		if err := <-flushed; err != nil {
			t.Fatalf("Flush after OpenRails came back: %v", err)
		}
		if got := f.settledOf(customer, "quota"); got != 3 {
			t.Fatalf("%d units settled, want 3", got)
		}
	})
}

// Close ends its lease past a refusal of a sync sent before it began.
func TestGateCloseCountsOnlySyncsSentAfterItBegins(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		sent, next := make(chan struct{}), make(chan struct{})
		f.set(func(f *fakeViews) { f.refuse = http.StatusBadRequest })
		held := f.holdNext(sent)
		if err := mustAdmit(t, g, work(customer, "open", 1)).Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-held
		closed := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			closed <- g.Close(ctx)
		}()
		synctest.Wait()
		// The held sync is refused while the next is in flight.
		held = f.holdNext(next)
		close(sent)
		<-held
		synctest.Wait()
		close(next)
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.leases[g.g.lease].closed {
			t.Fatal("Close returned with its lease open")
		}
	})
}

func TestGateRefusesUnpricedEvents(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{})
	f.money, f.balance = true, 1_000_000
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		p := work(customer, "unpriced", 1)
		p.Usage.EventType = "unpriced"
		// The first request learns the event type's pricing; no card prices
		// it, so every one is refused usage_not_priced.
		for i := range 3 {
			p.RequestID = fmt.Sprintf("unpriced-%d", i)
			if _, err := g.Admit(t.Context(), p); !hasCode(err, billing.CodeUsageNotPriced) {
				t.Fatalf("request %d of an event type no card prices: %v", i, err)
			}
		}
	})
}

func TestGateSettlesAnEarlierProcessBeforeItsViews(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 10_000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		customer := billing.CustomerID(uuid.New())
		earlier := newLeaseID()
		box := &memOutbox{items: map[string]billing.OutboxItem{}}
		at := time.Now().UTC()
		for n := int64(1); n <= 1500; n++ {
			item := billing.LeaseUsage{N: n, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
				Source: "test", SourceID: fmt.Sprintf("earlier-%d", n), Outcome: billing.UsageSucceeded, EventType: "work",
				Dimensions: map[string]int64{"units": 1}, Metadata: map[string]any{}, OccurredAt: at}
			box.items[fmt.Sprintf("%s/%d", earlier, n)] = billing.OutboxItem{LeaseID: earlier, Usage: item}
		}
		g := newTestGate(t, f, GateDeps{Outbox: box})
		defer closeGate(t, g)
		mustAdmit(t, g, work(customer, "first", 1))
		lines, err := g.ListCustomerLimits(t.Context(), customer)
		if err != nil {
			t.Fatal(err)
		}
		if lines[0].Remaining != 10_000-1500-1 {
			t.Fatalf("the first view shows %d remaining, want %d: the earlier process's usage first", lines[0].Remaining, 10_000-1500-1)
		}
	})
}

func TestGateOfflineShare(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	customer := billing.CustomerID(uuid.New())
	viewOf := func(lines ...billing.ViewLimit) *custView {
		v := newCustView(viewKey{customer, "USD"})
		v.mu.Lock()
		v.apply(billing.View{CustomerID: customer, Currency: "USD", AsOf: t0, Limits: lines}, t0)
		v.mu.Unlock()
		return v
	}
	balance := func(remaining, drain int64) billing.ViewLimit {
		return billing.ViewLimit{Limit: catalog.BalanceLimit, Unit: "USD", Overrun: catalog.OverrunRequireEstimate, Remaining: remaining, DrainPerHour: drain}
	}
	down := func(gates int) outage { return outage{since: t0.Add(time.Second), gates: gates} }
	cases := []struct {
		name    string
		v       *custView
		set     billing.LimitSet
		at      time.Time
		gates   int
		fits    int64
		refused int64
	}{
		// Remaining −50 on a credit line of 100 leaves 50: 25 a gate.
		{"credit line below zero", viewOf(balance(-50, 0)), billing.LimitSet{Limits: []string{catalog.BalanceLimit}, CreditLine: 100}, t0.Add(2 * time.Second), 2, 25, 26},
		// 40 left draining 10 an hour: after three hours 10 is left, 2 a gate.
		{"running charges", viewOf(balance(40, 10)), billing.LimitSet{Limits: []string{catalog.BalanceLimit}}, t0.Add(3 * time.Hour), 4, 2, 3},
		// The set caps the window at 10 where the line was built at 100.
		{"a spend window's cap", viewOf(balance(1000, 0), billing.ViewLimit{Limit: "spend:x", Unit: "USD", Overrun: catalog.OverrunRequireEstimate, Capacity: 100, Remaining: 100}),
			billing.LimitSet{Limits: []string{catalog.BalanceLimit, "spend:x"}, Capacities: []billing.LimitCapacity{{Limit: "spend:x", Capacity: 10}}}, t0.Add(2 * time.Second), 4, 2, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			costs := func(n int64) []int64 { return slices.Repeat([]int64{n}, len(c.set.Limits)) }
			if d := c.v.decide(c.set, costs(c.refused), c.at, down(c.gates)); d.verdict != verdictShort {
				t.Fatalf("%d past the share: verdict %d", c.refused, d.verdict)
			}
			if d := c.v.decide(c.set, costs(c.fits), c.at, down(c.gates)); d.verdict != verdictLocal {
				t.Fatalf("%d within the share: verdict %d", c.fits, d.verdict)
			}
		})
	}
	t.Run("a view older than the outage", func(t *testing.T) {
		v := viewOf(balance(100, 0))
		set := billing.LimitSet{Limits: []string{catalog.BalanceLimit}}
		late := outage{since: t0.Add(8 * time.Minute), gates: 1}
		if d := v.decide(set, []int64{1}, t0.Add(8*time.Minute+time.Second), late); d.verdict != verdictShort {
			t.Fatalf("an eight-minute-old view decided offline: verdict %d", d.verdict)
		}
	})
}

func TestGateAnswersAFirstViewInASyncWithoutUsage(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		warm(t, g, billing.CustomerID(uuid.New()))
		// Usage waits to be sent when a new customer's first request comes:
		// it is captured while a slow sync with nothing queued is out.
		busy := billing.CustomerID(uuid.New())
		warm(t, g, busy)
		queued := mustAdmit(t, g, work(busy, "queued", 3))
		f.set(func(f *fakeViews) { f.slow = time.Second })
		mustAdmit(t, g, work(busy, "probe", 1)).Release(t.Context())
		synctest.Wait()
		f.mu.Lock()
		before := len(f.syncs)
		f.mu.Unlock()
		if err := queued.Capture(t.Context(), capture(3)); err != nil {
			t.Fatal(err)
		}
		cold := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(cold, "first", 1)).Release(t.Context())
		flush(t, g)
		f.mu.Lock()
		syncs := slices.Clone(f.syncs[before:])
		f.mu.Unlock()
		answered, carried := -1, -1
		for i, p := range syncs {
			for _, v := range p.Views {
				if v.CustomerID == cold && answered < 0 {
					answered = i
					if len(p.Usage) > 0 {
						t.Fatalf("the sync answering the first view carried %d usage items", len(p.Usage))
					}
				}
			}
			for _, u := range p.Usage {
				if u.SourceID == "queued" && carried < 0 {
					carried = i
				}
			}
		}
		if answered < 0 || carried < 0 {
			t.Fatalf("first view in sync %d, queued usage in sync %d", answered, carried)
		}
		if carried != answered+1 {
			t.Fatalf("the queued usage went in sync %d, want the one after the first view's (%d)", carried, answered+1)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var units int64
		for k, v := range f.settled {
			if k.customer == busy {
				units += v
			}
		}
		if units != 4 {
			t.Fatalf("settled %d units of the busy customer, want 4 once", units)
		}
	})
}

func TestGateCarriesAFullBatchInAFirstViewSync(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1_000_000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		busy := billing.CustomerID(uuid.New())
		warm(t, g, busy)
		// Answers take a second, so the backlog is still queued when a new
		// customer's first request comes.
		f.set(func(f *fakeViews) { f.slow = time.Second })
		for i := range 2 * billing.MaxSyncUsageItems {
			u := billing.UsageParams{CustomerID: busy, Invoker: busy.String(), Currency: "USD", EventType: "work",
				Dimensions: map[string]int64{"units": 1}, Source: "test", SourceID: fmt.Sprintf("backlog-%d", i)}
			if err := g.Record(t.Context(), u); err != nil {
				t.Fatal(err)
			}
		}
		f.mu.Lock()
		before := len(f.syncs)
		f.mu.Unlock()
		cold := billing.CustomerID(uuid.New())
		mustAdmit(t, g, work(cold, "first", 1)).Release(t.Context())
		f.mu.Lock()
		syncs := slices.Clone(f.syncs[before:])
		f.mu.Unlock()
		for _, p := range syncs {
			for _, v := range p.Views {
				if v.CustomerID == cold {
					if len(p.Usage) != billing.MaxSyncUsageItems {
						t.Fatalf("the sync answering the first view carried %d items of a backlog, want %d", len(p.Usage), billing.MaxSyncUsageItems)
					}
					return
				}
			}
		}
		t.Fatal("no sync answered the first view")
	})
}
