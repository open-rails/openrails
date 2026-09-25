package nmimock

import (
	"net/http"
	"sync"
	"time"
)

// SetDecline sets the issuer's answer (see Card.Decline) for every stored
// card ending in last4.
func (m *Mock) SetDecline(last4, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.vaults {
		if v.Card.Last4 == last4 {
			v.Card.Decline = code
		}
		for i := range v.Extra {
			if v.Extra[i].Card.Last4 == last4 {
				v.Extra[i].Card.Decline = code
			}
		}
	}
}

// LoseSales makes the next n Direct Post requests (other than validate)
// fail in transit before the gateway sees them.
func (m *Mock) LoseSales(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lose = n
}

// DropSaleResponses makes the gateway process the next n Direct Post
// requests (other than validate) and lose each answer in transit.
func (m *Mock) DropSaleResponses(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drop = n
}

// DeclineValidations makes the issuer decline the next n card verifications
// (code 200), whatever the card.
func (m *Mock) DeclineValidations(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declineValid = n
}

// RefuseDuplicates makes the next n sales trip the duplicate check.
func (m *Mock) RefuseDuplicates(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.duplicate = n
}

// SetDuplicateWindow changes Options.DuplicateWindow.
func (m *Mock) SetDuplicateWindow(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.DuplicateWindow = d
}

// QueryUnavailable makes the Query API transaction report answer 503.
func (m *Mock) QueryUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queryDown = down
}

// FailScheduleUpdates makes the next n update_subscription requests fail.
func (m *Mock) FailScheduleUpdates(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failUpdates = n
}

// HideSales keeps the next n approved sales out of the Query API until
// Reveal, as its indexing lag does.
func (m *Mock) HideSales(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hide = n
}

// Reveal shows every hidden sale.
func (m *Mock) Reveal() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sales {
		s.Hidden = false
	}
}

type failure struct {
	match  func(*http.Request) bool
	status int
	mu     sync.Mutex
	left   int
}

func (f *failure) take() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.left == 0 {
		return false
	}
	f.left--
	return true
}

// FailRequests answers the next n matching requests with status (a 5xx)
// and an empty body, without the gateway acting.
func (m *Mock) FailRequests(match func(*http.Request) bool, status, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = append(m.failures, &failure{match: match, status: status, left: n})
}

// An Interceptor decides when, and whether, the gateway acts on a request
// (serve) and what its caller receives.
type Interceptor func(r *http.Request, serve func() *http.Response) (*http.Response, error)

type intercept struct {
	match func(*http.Request) bool
	fn    Interceptor
}

// Intercept routes matching requests through fn; the latest matching
// interceptor wins. match runs without the mock's lock held.
func (m *Mock) Intercept(match func(*http.Request) bool, fn Interceptor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intercepts = append(m.intercepts, &intercept{match: match, fn: fn})
}

// ClearIntercepts removes every interceptor and hold; parked requests stay
// parked until released.
func (m *Mock) ClearIntercepts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intercepts = nil
}

// HoldMode is what a held request has done at the gateway.
type HoldMode int

const (
	// HoldRequest parks before the gateway acts; a caller that gives up
	// leaves it never having arrived (a timeout before sending).
	HoldRequest HoldMode = iota
	// HoldCommit parks before the answer; a caller that gives up still
	// leaves the gateway having acted (commit, then a lost answer).
	HoldCommit
	// HoldResponse lets the gateway act, then parks its answer.
	HoldResponse
)

// Held is a hold on matching requests.
type Held struct {
	arrived, release chan struct{}
	arriveOnce       sync.Once
	releaseOnce      sync.Once
}

// Arrived closes when the first matching request is parked.
func (h *Held) Arrived() <-chan struct{} { return h.arrived }

// Release lets parked and later matching requests through.
func (h *Held) Release() { h.releaseOnce.Do(func() { close(h.release) }) }

// Hold parks matching requests until Release or their caller gives up; a
// never-released hold is a gateway timeout.
func (m *Mock) Hold(match func(*http.Request) bool, mode HoldMode) *Held {
	h := &Held{arrived: make(chan struct{}), release: make(chan struct{})}
	m.Intercept(match, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
		var res *http.Response
		if mode == HoldResponse {
			res = serve()
		}
		h.arriveOnce.Do(func() { close(h.arrived) })
		select {
		case <-h.release:
		case <-r.Context().Done():
			if mode == HoldCommit {
				serve()
			}
			return nil, r.Context().Err()
		}
		if res == nil {
			res = serve()
		}
		return res, nil
	})
	return h
}
