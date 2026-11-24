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
	// broken customers' views are refused (as a policy that fails to
	// resolve), deferred ones answered null; settleNone settles no new usage;
	// dropAdmit admits and loses the reply; slow delays answers.
	broken     map[billing.CustomerID]bool
	deferred   map[billing.CustomerID]bool
	settleNone bool
	dropAdmit  int
	slow       time.Duration
	// busyUntil answers every sync before it 503, as a server that could
	// not take its locks in time.
	busyUntil time.Time
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
	down, slow := f.down, f.slow
	f.mu.Unlock()
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
		if h := f.holds[item.RequestID]; h != nil {
			state := billing.AdmissionOpen
			out.Items = append(out.Items, billing.AdmissionVerdict{Status: 200, Admission: &billing.Admission{RequestID: item.RequestID, CustomerID: item.CustomerID,
				Currency: item.Currency, Allowed: true, State: &state}})
			continue
		}
		if f.money && item.Usage != nil && item.Usage.EventType != "work" {
			out.Items = append(out.Items, billing.AdmissionVerdict{Status: 422, Error: &billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeUsageNotPriced}})
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
	if f.dropAdmit > 0 {
		f.dropAdmit--
		writeFakeError(w, 503, "unavailable", "reply lost")
		return
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
	if now.Before(f.busyUntil) {
		writeFakeError(w, 503, billing.CodeServiceUnavailable, "the sync could not take its locks in time; retry it")
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
	ownSettled := map[fakeKey]int64{}
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
			res.Views = append(res.Views, f.view(vp.CustomerID, now, p.LeaseID, ownSettled))
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

func TestGateReleasesAHoldWhoseAnswerItLost(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 10}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		open := func(id string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			h := f.holds[id]
			return h != nil && h.open
		}
		// The server holds the last units and the answer is lost.
		f.set(func(f *fakeViews) { f.dropAdmit = 1 })
		if _, err := g.Admit(t.Context(), work(customer, "lost", 8)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a lost answer: %v", err)
		}
		if !open("lost") {
			t.Fatal("the fake made no hold")
		}
		time.Sleep(2500 * time.Millisecond)
		if open("lost") {
			t.Fatal("the hold of a lost answer is still held")
		}
		// A caller that stops waiting leaves its hold to the next sync and
		// the gate online.
		f.set(func(f *fakeViews) { f.slow = 500 * time.Millisecond })
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err := g.Admit(ctx, work(customer, "gave-up", 8))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("a caller's deadline: %v", err)
		}
		if g.g.offline().on() {
			t.Fatal("one caller's deadline took the gate offline")
		}
		time.Sleep(3 * time.Second)
		f.set(func(f *fakeViews) { f.slow = 0 })
		time.Sleep(2 * time.Second)
		if open("gave-up") {
			t.Fatal("the hold of a caller that gave up is still held")
		}
		// A retry under the request id before the next sync adopts its hold.
		time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 100*time.Millisecond)))
		f.set(func(f *fakeViews) { f.slow = 500 * time.Millisecond })
		ctx, cancel = context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err = g.Admit(ctx, work(customer, "retried", 8))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller's deadline: %v", err)
		}
		f.set(func(f *fakeViews) { f.slow = 0 })
		tk := mustAdmit(t, g, work(customer, "retried", 8))
		time.Sleep(2500 * time.Millisecond)
		if !open("retried") {
			t.Fatal("the hold a retry adopted was released")
		}
		if err := tk.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGateReportsACapturedTicketUntilItSettles(t *testing.T) {
	f := newFakeViews(map[string]fakeLimit{"quota": {capacity: 1000}})
	f.pool(t, 1)
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t, f, GateDeps{})
		defer closeGate(t, g)
		customer := billing.CustomerID(uuid.New())
		warm(t, g, customer)
		long := mustAdmit(t, g, work(customer, "long", 7))
		time.Sleep(3500 * time.Millisecond)
		if got := reportedOf(f, g.g.lease); got != 7 {
			t.Fatalf("reported %d, want the open ticket's 7", got)
		}
		// Captured, and carried by a sync that settles none of it.
		f.set(func(f *fakeViews) { f.settleNone = true })
		if err := long.Capture(t.Context(), capture(7)); err != nil {
			t.Fatal(err)
		}
		// The capture's own sync carries it for the first time.
		time.Sleep(100 * time.Millisecond)
		if got := reportedOf(f, g.g.lease); got != 7 {
			t.Fatalf("reported %d while its usage is unsettled, want 7", got)
		}
		f.set(func(f *fakeViews) { f.settleNone = false })
		flush(t, g)
		time.Sleep(3 * time.Second)
		if got := reportedOf(f, g.g.lease); got != 0 {
			t.Fatalf("reported %d once settled, want 0", got)
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

func TestGateSendsUnpricedEventsToTheServer(t *testing.T) {
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
		// The first request learns the event type's pricing; every one is
		// answered as Admit answers it.
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
		v.apply(billing.View{CustomerID: customer, Currency: "USD", AsOf: t0, Limits: lines}, t0, 1, 2)
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

func TestGateFirstMarginIsRecentSpend(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	v := newCustView(viewKey{customer, "USD"})
	now := time.Now()
	v.mu.Lock()
	v.apply(billing.View{CustomerID: customer, Currency: "USD", Limits: []billing.ViewLimit{{Limit: catalog.BalanceLimit, Unit: "USD", Remaining: 1_000_000, Recent: 60_000}}}, now, 1, 1)
	l := v.lines[catalog.BalanceLimit]
	v.mu.Unlock()
	if got := l.margin(10); got != 4000 {
		t.Fatalf("first margin %d, want 4 intervals of a minute's spend per second", got)
	}
}
