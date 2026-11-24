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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
)

// fakeViews speaks the view protocol: limits count the event's "units";
// with money, balance costs 1000 micros a unit. Usage settles in order, each
// sync replaces its lease's in-flight report, and a lease's admission holds
// a request on the server against everything settled, held and reported.
type fakeViews struct {
	mu       sync.Mutex
	merchant billing.MerchantID
	limits   map[string]fakeLimit
	money    bool
	balance  int64
	settled  map[fakeKey]int64
	inFlight map[billing.LeaseID]map[fakeKey]int64
	holds    map[string]*fakeHold
	leases   map[billing.LeaseID]*fakeLease
	down     bool
	drop     int
	perSync  int
	syncs    []billing.SyncLeasesParams
	admits   []billing.AdmitParams
	clients  []*Client
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

type fakeHold struct {
	lease    billing.LeaseID
	customer billing.CustomerID
	costs    map[string]int64
	open     bool
}

type fakeLease struct {
	through int64
	items   map[int64]string
	last    time.Time
	closed  bool
}

func newFakeViews(limits map[string]fakeLimit) *fakeViews {
	return &fakeViews{merchant: billing.MerchantID(uuid.New()), limits: limits, settled: map[fakeKey]int64{},
		inFlight: map[billing.LeaseID]map[fakeKey]int64{}, holds: map[string]*fakeHold{}, leases: map[billing.LeaseID]*fakeLease{}}
}

func (f *fakeViews) RoundTrip(r *http.Request) (*http.Response, error) {
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
	for _, h := range f.holds {
		if h.open && h.customer == customer && f.live(h.lease, now) {
			held += h.costs[limit]
		}
	}
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
	case "/v1/app/admissions":
		f.admit(w, r)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeViews) admit(w http.ResponseWriter, r *http.Request) {
	var p billing.AdmitBatchParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeFakeError(w, 400, billing.CodeInvalidParam, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	out := billing.AdmitBatchResult{}
	for _, item := range p.Items {
		f.admits = append(f.admits, item)
		if item.LeaseID == nil || f.leases[*item.LeaseID] == nil {
			code := billing.CodeLeaseNotHeld
			out.Items = append(out.Items, billing.AdmissionVerdict{Status: 409, Error: &billing.ErrorDetails{Code: code}})
			continue
		}
		units := item.Usage.Dimensions["units"]
		costs := map[string]int64{}
		refused := ""
		for _, k := range f.keys() {
			c := f.cost(k, units)
			settled, held := f.usedLocked(item.CustomerID, k, now, billing.LeaseID{})
			left := f.capacity(k) - settled - held
			if f.limits[k].overrun == catalog.OverrunAdmitWhilePositive && left > 0 || left >= c {
				costs[k] = c
				continue
			}
			if refused == "" {
				refused = k
			}
		}
		a := &billing.Admission{RequestID: item.RequestID, CustomerID: item.CustomerID, Currency: item.Currency}
		if refused != "" {
			code := billing.DenyLimitReached
			blocked := billing.AdmissionBlockedByLimit
			a.BlockedBy, a.DenyCode, a.Limit = &blocked, &code, &refused
			out.Items = append(out.Items, billing.AdmissionVerdict{Status: 429, Admission: a})
			continue
		}
		state := billing.AdmissionOpen
		a.Allowed, a.State = true, &state
		f.holds[item.RequestID] = &fakeHold{lease: *item.LeaseID, customer: item.CustomerID, costs: costs, open: true}
		out.Items = append(out.Items, billing.AdmissionVerdict{Status: 200, Admission: a})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
	ownSettled := map[fakeKey]int64{}
	fresh := 0
	for i, u := range p.Usage {
		if f.perSync > 0 && fresh == f.perSync && u.N > lease.through {
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
		if u.RequestID != nil {
			if h := f.holds[*u.RequestID]; h != nil {
				h.open = false
			}
		}
		for _, k := range f.keys() {
			win, _, _ := f.window(k, at)
			c := f.cost(k, u.Dimensions["units"])
			f.settled[fakeKey{u.CustomerID, k, win}] += c
			ownSettled[fakeKey{u.CustomerID, k, 0}] += c
		}
	}
	res.Usage.ThroughN = lease.through
	for _, id := range p.Releases {
		if h := f.holds[id]; h != nil && h.lease == p.LeaseID {
			h.open = false
		}
	}
	if p.Close || lease.closed {
		lease.closed = true
		delete(f.inFlight, p.LeaseID)
		for _, h := range f.holds {
			if h.lease == p.LeaseID {
				h.open = false
			}
		}
	} else {
		lines := map[fakeKey]int64{}
		for _, l := range p.InFlight {
			lines[fakeKey{l.CustomerID, l.Limit, 0}] += l.Amount
		}
		f.inFlight[p.LeaseID] = lines
	}
	viewed := map[billing.CustomerID]bool{}
	priced := false
	for _, vp := range p.Views {
		set := &billing.LimitSet{CustomerID: vp.CustomerID, Currency: vp.Currency, Limits: f.keys(), BalanceDenyCode: billing.DenyInsufficientBalance}
		res.LimitSets = append(res.LimitSets, set)
		if !viewed[vp.CustomerID] {
			viewed[vp.CustomerID] = true
			res.Views = append(res.Views, f.view(vp.CustomerID, now, p.LeaseID, ownSettled))
		}
		if vp.EventType != nil && !priced {
			priced = true
			res.Pricing = append(res.Pricing, f.pricing())
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

func (f *fakeViews) view(customer billing.CustomerID, now time.Time, except billing.LeaseID, own map[fakeKey]int64) billing.View {
	v := billing.View{CustomerID: customer, Currency: "USD", AsOf: now.UTC(), Limits: []billing.ViewLimit{}}
	for _, k := range f.keys() {
		settled, held := f.usedLocked(customer, k, now, except)
		unit := "units"
		if k == catalog.BalanceLimit {
			unit = "USD"
		}
		line := billing.ViewLimit{Limit: k, Unit: unit, Overrun: catalog.OverrunRequireEstimate, Capacity: f.capacity(k), Used: settled, Held: held,
			Remaining: f.capacity(k) - settled - held, Settled: own[fakeKey{customer, k, 0}]}
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

func (f *fakeViews) pricing() billing.ViewPricing {
	p := billing.ViewPricing{Currency: "USD", EventType: "work", Counts: []billing.ViewCount{}, RateCards: []billing.ViewRateCard{}, PriceBounds: []billing.PriceBound{}}
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
		// daily has 59 left; near zero the server decides, and it counts the
		// gate's tickets only once they are reported: the gate alone holds
		// them, so the limit is never passed.
		if admitted > 59 {
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
		if tk.held {
			t.Fatal("a request far from the limit was decided on the server")
		}
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

func TestGateDecidesNearZeroOnTheServer(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 10}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		for i := range 4 {
			tk := mustAdmit(t, g, work(customer, fmt.Sprintf("far-%d", i), 1))
			if err := tk.Capture(t.Context(), capture(1)); err != nil {
				t.Fatal(err)
			}
		}
		flush(t, g)
		f.mu.Lock()
		before := len(f.admits)
		f.mu.Unlock()
		if before != 0 {
			t.Fatalf("%d requests went to the server far from zero", before)
		}
		admitted := 0
		for i := range 8 {
			tk, err := g.Admit(t.Context(), work(customer, fmt.Sprintf("near-%d", i), 2))
			if err != nil {
				t.Fatal(err)
			}
			if tk.Allowed() {
				admitted++
				if err := tk.Capture(t.Context(), capture(2)); err != nil {
					t.Fatal(err)
				}
			}
		}
		flush(t, g)
		if got := f.settledOf(customer, "quota"); got > 10 {
			t.Fatalf("settled %d past capacity 10", got)
		}
		f.mu.Lock()
		held := len(f.admits)
		lease := f.admits[0].LeaseID
		f.mu.Unlock()
		if held == 0 || lease == nil || *lease != g.g.lease {
			t.Fatalf("near zero: %d server admissions, lease %v", held, lease)
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
	lease := billing.LeaseID(uuid.New())
	for name, p := range map[string]billing.AdmitParams{
		"no request id":  {CustomerID: customer, Currency: "USD"},
		"no customer":    {RequestID: "r", Currency: "USD"},
		"no currency":    {RequestID: "r", CustomerID: customer},
		"usage and cost": {RequestID: "r", CustomerID: customer, Currency: "USD", EstimatedAmount: 1, Usage: &billing.UsageEstimate{EventType: "work"}},
		"a lease":        {RequestID: "r", CustomerID: customer, Currency: "USD", LeaseID: &lease},
	} {
		if _, err := g.Admit(t.Context(), p); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	if _, err := NewGate(nil, GateDeps{}); err == nil {
		t.Error("a gate with no handle")
	}
}

func TestGateMarginFollowsWhatOthersAdd(t *testing.T) {
	l := &lineState{seen: true, others: []int64{2, 9, 4}}
	if got := l.margin(1); got != 36 {
		t.Fatalf("margin %d, want 4 intervals of the most others added (9)", got)
	}
	l = &lineState{money: true, line: billing.ViewLimit{Recent: 60_000}}
	if got := l.margin(10); got != 4000 {
		t.Fatalf("first margin %d, want 4 intervals of a minute's spend per second", got)
	}
}
