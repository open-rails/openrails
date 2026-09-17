//go:build integration

package integrationharness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// StripeWriteMode scripts how the loopback Stripe answers the next mutating
// request.
type StripeWriteMode string

const (
	// StripeWriteApply applies the write and answers the object.
	StripeWriteApply StripeWriteMode = "apply"
	// StripeWriteLostAfterLanding applies the write, stores the answer under
	// the idempotency key as Stripe does, and answers 502 once: the response
	// was lost after Stripe committed.
	StripeWriteLostAfterLanding StripeWriteMode = "lost_after_landing"
	// StripeWriteLostBeforeLanding answers 502 once and applies nothing: the
	// request never reached Stripe.
	StripeWriteLostBeforeLanding StripeWriteMode = "lost_before_landing"
	// StripeWriteDecline answers a stored 402 card decline.
	StripeWriteDecline StripeWriteMode = "decline"
)

// StripeRequest is one request the gateway recorded.
type StripeRequest struct {
	Method, Path, IdempotencyKey string
	Form                         url.Values
}

// StripeSubscription is the gateway's view of one subscription.
type StripeSubscription struct {
	ID, ItemID, PriceID, ScheduleID string
	Metadata                        map[string]string
	PeriodStart, PeriodEnd          int64
}

// FakeStripeGateway is a loopback Stripe Billing API for tier changes: the
// subscription read and update, and schedule create/update/read, with
// Stripe's idempotency-key replay of a completed answer (2xx or 4xx; a 5xx
// stores nothing). It is a fake provider: nothing here proves live Stripe
// behavior.
type FakeStripeGateway struct {
	URL string

	server    *httptest.Server
	mu        sync.Mutex
	mode      StripeWriteMode
	subs      map[string]*StripeSubscription
	schedules map[string]map[string]any
	stored    map[string]storedStripeAnswer
	requests  []StripeRequest
	created   int
}

type storedStripeAnswer struct {
	status int
	body   []byte
}

// NewFakeStripeGateway starts the gateway applying writes. Point a runtime at
// it with config.ProviderSandbox{StripeAPIURL: g.URL}.
func NewFakeStripeGateway(t testing.TB) *FakeStripeGateway {
	t.Helper()
	g := &FakeStripeGateway{mode: StripeWriteApply, subs: map[string]*StripeSubscription{}, schedules: map[string]map[string]any{}, stored: map[string]storedStripeAnswer{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	g.URL = g.server.URL
	t.Cleanup(g.server.Close)
	return g
}

// SetMode scripts the next mutating request.
func (g *FakeStripeGateway) SetMode(mode StripeWriteMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
}

// DeclareSubscription registers a live subscription billing priceID.
func (g *FakeStripeGateway) DeclareSubscription(id, priceID string, periodStart, periodEnd time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.subs[id] = &StripeSubscription{ID: id, ItemID: "si_" + strings.TrimPrefix(id, "sub_"), PriceID: priceID, Metadata: map[string]string{}, PeriodStart: periodStart.Unix(), PeriodEnd: periodEnd.Unix()}
}

// Subscription returns the gateway's current view of the subscription.
func (g *FakeStripeGateway) Subscription(id string) StripeSubscription {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := *g.subs[id]
	s.Metadata = map[string]string{}
	for k, v := range g.subs[id].Metadata {
		s.Metadata[k] = v
	}
	return s
}

// Posts returns the mutating requests whose path starts with prefix.
func (g *FakeStripeGateway) Posts(prefix string) []StripeRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []StripeRequest
	for _, r := range g.requests {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (g *FakeStripeGateway) subscriptionJSON(s *StripeSubscription) map[string]any {
	var schedule any
	if s.ScheduleID != "" {
		schedule = s.ScheduleID
	}
	return map[string]any{"id": s.ID, "object": "subscription", "status": "active", "metadata": s.Metadata, "schedule": schedule, "latest_invoice": "in_" + strings.TrimPrefix(s.ID, "sub_"),
		"items": map[string]any{"data": []any{map[string]any{"id": s.ItemID, "price": map[string]any{"id": s.PriceID}, "current_period_start": s.PeriodStart, "current_period_end": s.PeriodEnd}}}}
}

func stripeAnswer(w http.ResponseWriter, status int, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func stripeError(w http.ResponseWriter, status int, message string) {
	stripeAnswer(w, status, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message, "code": "resource_missing"}})
}

func (g *FakeStripeGateway) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_ = r.ParseForm()
	key := r.Header.Get(stripeapi.IdempotencyKeyHeader)
	g.requests = append(g.requests, StripeRequest{Method: r.Method, Path: r.URL.Path, IdempotencyKey: key, Form: r.PostForm})
	if r.Method == http.MethodGet {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			if s, ok := g.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]; ok {
				stripeAnswer(w, http.StatusOK, g.subscriptionJSON(s))
				return
			}
		case strings.HasPrefix(r.URL.Path, "/v1/subscription_schedules/"):
			if sch, ok := g.schedules[strings.TrimPrefix(r.URL.Path, "/v1/subscription_schedules/")]; ok {
				stripeAnswer(w, http.StatusOK, sch)
				return
			}
		}
		stripeError(w, http.StatusNotFound, "No such object")
		return
	}
	if r.Method != http.MethodPost {
		stripeError(w, http.StatusNotImplemented, "unexpected "+r.Method+" "+r.URL.Path)
		return
	}
	if stored, ok := g.stored[key]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stored.status)
		_, _ = w.Write(stored.body)
		return
	}
	mode := g.mode
	g.mode = StripeWriteApply
	switch mode {
	case StripeWriteLostBeforeLanding:
		stripeAnswer(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream failure"}})
		return
	case StripeWriteDecline:
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "card_error", "code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds."}})
		g.stored[key] = storedStripeAnswer{status: http.StatusPaymentRequired, body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write(body)
		return
	}
	var answer map[string]any
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		s, ok := g.subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]
		if !ok {
			stripeError(w, http.StatusNotFound, "No such subscription")
			return
		}
		if s.ScheduleID != "" {
			stripeError(w, http.StatusBadRequest, "This subscription is managed by a subscription schedule")
			return
		}
		s.PriceID = r.PostForm.Get("items[0][price]")
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "metadata[") {
				s.Metadata[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
			}
		}
		if r.PostForm.Get("billing_cycle_anchor") == "now" {
			s.PeriodStart = time.Now().Unix()
			s.PeriodEnd = s.PeriodStart + 30*24*3600
		}
		answer = g.subscriptionJSON(s)
	case r.URL.Path == "/v1/subscription_schedules":
		s, ok := g.subs[r.PostForm.Get("from_subscription")]
		if !ok || s.ScheduleID != "" {
			stripeError(w, http.StatusBadRequest, "The subscription already has a schedule or does not exist")
			return
		}
		g.created++
		id := fmt.Sprintf("sub_sched_%d", g.created)
		s.ScheduleID = id
		g.schedules[id] = map[string]any{"id": id, "object": "subscription_schedule", "subscription": s.ID, "status": "active", "end_behavior": "release", "metadata": map[string]string{},
			"phases": []any{map[string]any{"start_date": s.PeriodStart, "end_date": s.PeriodEnd, "items": []any{map[string]any{"price": s.PriceID, "quantity": 1}}}}}
		answer = g.schedules[id]
	case strings.HasPrefix(r.URL.Path, "/v1/subscription_schedules/"):
		sch, ok := g.schedules[strings.TrimPrefix(r.URL.Path, "/v1/subscription_schedules/")]
		if !ok {
			stripeError(w, http.StatusNotFound, "No such schedule")
			return
		}
		phase := func(i int) map[string]any {
			p := "phases[" + strconv.Itoa(i) + "]"
			start, _ := strconv.ParseInt(r.PostForm.Get(p+"[start_date]"), 10, 64)
			end, _ := strconv.ParseInt(r.PostForm.Get(p+"[end_date]"), 10, 64)
			return map[string]any{"start_date": start, "end_date": end, "items": []any{map[string]any{"price": r.PostForm.Get(p + "[items][0][price]"), "quantity": 1}}}
		}
		meta := map[string]string{}
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "metadata[") {
				meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
			}
		}
		sch["phases"], sch["end_behavior"], sch["metadata"] = []any{phase(0), phase(1)}, r.PostForm.Get("end_behavior"), meta
		answer = sch
	default:
		stripeError(w, http.StatusNotImplemented, "unexpected "+r.Method+" "+r.URL.Path)
		return
	}
	body, _ := json.Marshal(answer)
	g.stored[key] = storedStripeAnswer{status: http.StatusOK, body: body}
	if mode == StripeWriteLostAfterLanding {
		stripeAnswer(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream failure after commit"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
