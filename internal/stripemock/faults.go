package stripemock

import (
	"net/http"
	"sync"
	"time"
)

// SetClock replaces Options.Clock. The clock must be safe for concurrent use.
func (m *Mock) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// SetDecline sets the issuer's answer (see Card.Decline) for future charges
// on every card ending in last4.
func (m *Mock) SetDecline(last4, decline string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, pm := range m.methods {
		if pm["card"].(Object)["last4"] == last4 {
			m.declines[id] = decline
		}
	}
}

// SetMethodDecline sets the issuer's answer for future charges on one
// payment method.
func (m *Mock) SetMethodDecline(pm, decline string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declines[pm] = decline
}

// DelayIntentVisibility models a list that has not caught up with a
// completed create: reads by id see a PaymentIntent at once, the list after
// delay.
func (m *Mock) DelayIntentVisibility(delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.visibilityDelay = delay
}

// LoseSubmissions makes the next n PaymentIntent creates fail in transit,
// never reaching Stripe.
func (m *Mock) LoseSubmissions(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lose = n
}

// PaymentIntentListUnavailable makes the PaymentIntent list answer 503.
func (m *Mock) PaymentIntentListUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listDown = down
}

// ChargeListUnavailable makes the charge list answer 503.
func (m *Mock) ChargeListUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chargesDown = down
}

// SubscriptionWritesUnavailable makes every subscription write answer 503.
func (m *Mock) SubscriptionWritesUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subsDown = down
}

// PriceReadsUnavailable makes every Price read answer 503.
func (m *Mock) PriceReadsUnavailable(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pricesDown = down
}

// An Interceptor decides when, and whether, Stripe acts on a request (serve)
// and what its caller receives.
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

// HoldMode is what a held request has done at Stripe.
type HoldMode int

const (
	// HoldRequest parks before Stripe acts; a caller that gives up leaves it
	// never having arrived (a timeout before sending).
	HoldRequest HoldMode = iota
	// HoldCommit parks before the answer; a caller that gives up still
	// leaves Stripe having acted (commit, then a lost answer).
	HoldCommit
	// HoldResponse lets Stripe act, then parks its answer.
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
// never-released hold is a Stripe timeout.
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
			if mode == HoldCommit && res == nil {
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

// SetPaymentIntentStatus moves a PaymentIntent to status, as Stripe's own
// processing does.
func (m *Mock) SetPaymentIntentStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intents[id]["status"] = status
}

// HidePaymentIntent keeps a PaymentIntent out of the list for d on the mock
// clock; reads by id still see it.
func (m *Mock) HidePaymentIntent(id string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.visibleAt[id] = m.now().Add(d)
}
