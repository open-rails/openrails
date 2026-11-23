package openrails

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// Gate admits requests against a merchant's usage limits in the host's
// process. Each customer's capacity is set aside in holds owned by the
// gate's lease (billing.Hold): a warm decision takes the request's cost from
// every limit of its hold or from none, with no I/O, so it holds while
// OpenRails is unreachable. A cold customer, or a hold short of what its
// demand needs, waits for one SyncLeases call. The gate syncs its lease on a
// fixed cadence while it lives, carrying the usage its tickets captured; the
// server prices that usage exactly. It works over the remote Client and the
// embedded engine's alike. Use one gate per merchant handle and process.
type Gate struct {
	c      *Client
	local  *localGate
	shared *sharedGate

	mu     sync.Mutex
	closed bool
}

// GateConfig configures a Gate. Nothing in it tunes timing, sizes or
// tolerance: those follow from traffic.
type GateConfig struct {
	// Redis makes the merchant's processes one gate: one lease and one hold
	// per customer and limit set, kept in Redis, decided by one script per
	// request. A process that cannot reach Redis admits from its own lease
	// meanwhile. Losing Redis loses the usage it had not yet sent, which is
	// never billed. OpenRails does not close it.
	Redis redis.UniversalClient
	// Outbox keeps captured usage durably until OpenRails settles it, so a
	// restarted process resends it; without it a crash loses unsent usage,
	// which is never billed. Not with Redis, whose stream is durable.
	Outbox UsageOutbox
}

// UsageOutbox stores a gate's captured usage until OpenRails settles it.
// Put returns before Capture does; Pending is read when a gate starts and
// resent against the original leases; Ack follows settlement. It must be
// safe for concurrent use.
type UsageOutbox interface {
	Put(ctx context.Context, items []OutboxItem) error
	Pending(ctx context.Context) ([]OutboxItem, error)
	Ack(ctx context.Context, items []billing.LeaseUsageRef) error
}

// OutboxItem is one usage item of a lease.
type OutboxItem struct {
	LeaseID billing.LeaseID    `json:"lease_id"`
	Usage   billing.LeaseUsage `json:"usage"`
}

// ErrUnavailable is a decision the gate could not make because OpenRails
// could not be reached and no hold (or its safe amount) covered it. It is
// also billing.ErrUnreachable.
var ErrUnavailable = fmt.Errorf("openrails: gate: OpenRails is unavailable: %w", billing.ErrUnreachable)

// errGateClosed is a call on a closed gate.
var errGateClosed = errors.New("openrails: gate: closed")

// Gate timing and bounds. None is a setting: correctness holds for any
// value, and only speed depends on them.
const (
	// syncTimeout bounds one SyncLeases call, well inside the lease's life.
	syncTimeout = limits.LeaseLifetime / 2
	// maxGateHolds bounds the holds a gate keeps; past it the least recently
	// used are returned on the next sync.
	maxGateHolds = 10_000
	// maxGateLimitSets bounds the cached invoker-attributes to limit-set map.
	maxGateLimitSets = 100_000
	// maxColdRounds bounds the syncs one request waits through for a hold
	// that fits it.
	maxColdRounds = 20
)

// NewGate starts a gate over c, a remote or embedded merchant handle. Its
// lease is created on the first sync.
func NewGate(c *Client, cfg GateConfig) (*Gate, error) {
	switch {
	case c == nil:
		return nil, fmt.Errorf("openrails: NewGate needs a merchant handle")
	case cfg.Redis != nil && cfg.Outbox != nil:
		return nil, fmt.Errorf("openrails: GateConfig takes Redis or Outbox, not both")
	}
	g := &Gate{c: c}
	if cfg.Redis != nil {
		g.local = newLocalGate(c, nil, false)
		g.shared = newSharedGate(c, cfg.Redis, g.local)
		g.shared.start()
		return g, nil
	}
	g.local = newLocalGate(c, cfg.Outbox, true)
	return g, nil
}

func (g *Gate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// Admit decides one request, as Client.Admit would, against the gate's
// holds. A refusal is a verdict (Ticket.Refusal), not an error: limit_reached
// names the limit and when it resets, money refusals keep their deny codes,
// and capacity_held, capacity other gates hold, is waited out within ctx.
// The error is for bad input, ctx, or ErrUnavailable. A request that declares
// AccrualRateDeltaPerHour is admitted by the server as a request hold.
// ExpiresAt is not needed otherwise: a ticket lives while the gate syncs.
func (g *Gate) Admit(ctx context.Context, p billing.AdmitParams) (*Ticket, error) {
	if g.isClosed() {
		return nil, errGateClosed
	}
	if err := normalizeGateAdmit(&p); err != nil {
		return nil, err
	}
	if p.AccrualRateDeltaPerHour != 0 {
		return g.admitRemote(ctx, p)
	}
	if g.shared != nil {
		return g.shared.admit(ctx, g, p)
	}
	return g.local.admit(ctx, g, p)
}

// Consume admits a request whose cost is known and captures it at once with
// its estimate as the usage.
func (g *Gate) Consume(ctx context.Context, p billing.AdmitParams) (*Ticket, error) {
	if p.Usage == nil {
		return nil, invalidParam("usage", "Consume states the request's usage")
	}
	t, err := g.Admit(ctx, p)
	if err != nil || !t.Allowed() {
		return t, err
	}
	u := billing.CaptureUsage{EventType: p.Usage.EventType, Resource: p.Resource, Metadata: p.Usage.Metadata, Source: p.Source, SourceID: p.RequestID, Dimensions: p.Usage.Dimensions}
	if err := t.Capture(ctx, u); err != nil {
		return t, err
	}
	return t, nil
}

// Record reports usage no admission held, sent on the next sync. It counts
// toward the customer's limits and is billed as the server prices it.
func (g *Gate) Record(ctx context.Context, u billing.UsageParams) error {
	if g.isClosed() {
		return errGateClosed
	}
	item, err := recordItem(u)
	if err != nil {
		return err
	}
	if g.shared != nil {
		return g.shared.record(ctx, item)
	}
	return g.local.record(ctx, item)
}

// Limits is where the customer stands on each limit, for display: the values
// of the last sync that reported them (or a read when none did), less the
// usage the gate has not yet sent. It never adds spendable capacity.
func (g *Gate) Limits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	if customer.IsZero() {
		return nil, invalidParam("customer_id", "customer_id is required")
	}
	if g.shared != nil {
		return g.shared.limits(ctx, customer)
	}
	return g.local.limits(ctx, customer)
}

// Flush syncs now and returns once every usage item captured before the call
// is settled, or with ErrUnavailable or ctx's error. It returns the errors of
// items OpenRails refused since the last Flush; those posted nothing.
func (g *Gate) Flush(ctx context.Context) error {
	if g.shared != nil {
		return errors.Join(g.shared.flush(ctx), g.local.flushIfStarted(ctx))
	}
	return g.local.flush(ctx, false)
}

// Close sends pending usage, returns the gate's holds and ends its lease
// (SyncLeasesParams.Close), then stops syncing. With Redis, a process leaves
// the group, and the group's lease ends with the last process to close; a
// lease that cannot be closed expires. Close after the gate's tickets are
// captured or released: a ticket captured after Close is refused.
func (g *Gate) Close(ctx context.Context) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	var err error
	if g.shared != nil {
		err = g.shared.close(ctx)
	}
	return errors.Join(err, g.local.close(ctx))
}

// Ticket is one decision of a gate. An allowed ticket is captured with the
// request's usage or released; one that is neither stays held while the gate
// lives.
type Ticket struct {
	gate     *Gate
	params   billing.AdmitParams
	refusal  *billing.Refusal
	limitSet string

	mu    sync.Mutex
	state ticketState

	local  *localTicket
	shared *sharedTicket
}

type ticketState uint8

const (
	ticketOpen ticketState = iota
	ticketCaptured
	ticketReleased
)

// Allowed reports whether the request was admitted.
func (t *Ticket) Allowed() bool { return t != nil && t.refusal == nil }

// Refusal is why the request was refused, nil when it was admitted.
func (t *Ticket) Refusal() *billing.Refusal {
	if t == nil || t.refusal == nil {
		return nil
	}
	out := *t.refusal
	return &out
}

// Capture reports the request's usage, sent on the next sync (with an
// Outbox, stored before Capture returns). Usage is today's CaptureUsage: its
// event type is the admitted one, Source defaults to "admit" and SourceID to
// the request id. A dynamic card charges the admission's stated amount.
func (t *Ticket) Capture(ctx context.Context, u billing.CaptureUsage) error {
	if !t.Allowed() {
		return invalidParam("request_id", "a refused request has nothing to capture")
	}
	if err := t.captureUsage(&u); err != nil {
		return err
	}
	t.mu.Lock()
	if t.state != ticketOpen {
		t.mu.Unlock()
		return &billing.StatusError{Status: http.StatusConflict, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeResourceConflict, Message: "the ticket was already captured or released"}}
	}
	t.state = ticketCaptured
	t.mu.Unlock()
	var err error
	switch {
	case t.local != nil:
		err = t.local.capture(ctx, t, u)
	case t.shared != nil:
		err = t.shared.capture(ctx, t, u)
	default:
		err = t.gate.captureRemote(ctx, t, u)
	}
	var nc notCaptured
	if errors.As(err, &nc) {
		t.mu.Lock()
		t.state = ticketOpen
		t.mu.Unlock()
		return nc.error
	}
	return err
}

// Release gives the request's hold back when its work failed; it is a no-op
// once captured or released.
func (t *Ticket) Release(ctx context.Context) error {
	if !t.Allowed() {
		return nil
	}
	t.mu.Lock()
	if t.state != ticketOpen {
		t.mu.Unlock()
		return nil
	}
	t.state = ticketReleased
	t.mu.Unlock()
	switch {
	case t.local != nil:
		t.local.release()
		return nil
	case t.shared != nil:
		return t.shared.release(ctx)
	default:
		return t.gate.releaseRemote(ctx, t)
	}
}

// captureUsage fills a capture's defaults and checks it against the
// admission.
func (t *Ticket) captureUsage(u *billing.CaptureUsage) error {
	u.EventType = strings.TrimSpace(u.EventType)
	admitted := ""
	if t.params.Usage != nil {
		admitted = strings.TrimSpace(t.params.Usage.EventType)
	}
	switch {
	case u.EventType == "" && admitted == "":
		return invalidParam("usage.event_type", "usage.event_type is required")
	case u.EventType == "":
		u.EventType = admitted
	case admitted != "" && u.EventType != admitted:
		return invalidParam("usage.event_type", fmt.Sprintf("the capture's event type must be the admitted %q", admitted))
	}
	if u.Source == "" {
		u.Source = "admit"
	}
	if u.SourceID == "" {
		u.SourceID = t.params.RequestID
	}
	return nil
}

// statedAmount is what a dynamic card charges the request's usage.
func statedAmount(p billing.AdmitParams) *int64 {
	switch {
	case p.Usage != nil && p.Usage.Amount != nil:
		v := *p.Usage.Amount
		return &v
	case p.Usage == nil && p.EstimatedAmount > 0:
		v := p.EstimatedAmount
		return &v
	}
	return nil
}

// captureItem is a ticket's usage as a lease reports it, numbered later.
func captureItem(p billing.AdmitParams, limitSet *string, u billing.CaptureUsage, now time.Time) billing.LeaseUsage {
	return billing.LeaseUsage{
		CustomerID: p.CustomerID, LimitSet: limitSet, Invoker: p.Invoker, InvokerType: p.InvokerType, Currency: p.Currency,
		Source: u.Source, SourceID: u.SourceID, Outcome: billing.UsageSucceeded, EventType: u.EventType,
		Dimensions: nonNilDims(u.Dimensions), Metadata: nonNilMeta(u.Metadata), Amount: statedAmount(p), OccurredAt: now.UTC(),
	}
}

func recordItem(u billing.UsageParams) (billing.LeaseUsage, error) {
	u.Currency = normalizeCurrency(u.Currency)
	u.EventType = strings.TrimSpace(u.EventType)
	switch {
	case u.CustomerID.IsZero():
		return billing.LeaseUsage{}, invalidParam("customer_id", "customer_id is required")
	case u.Currency == "":
		return billing.LeaseUsage{}, invalidParam("currency", "currency is required")
	case u.EventType == "":
		return billing.LeaseUsage{}, invalidParam("event_type", "event_type is required")
	case strings.TrimSpace(u.Source) == "" || strings.TrimSpace(u.SourceID) == "":
		return billing.LeaseUsage{}, invalidParam("source_id", "source and source_id are required")
	case u.Amount != nil && *u.Amount < 0:
		return billing.LeaseUsage{}, invalidParam("amount", "amount must be nonnegative")
	}
	if u.InvokerType == "" {
		u.InvokerType = billing.InvokerTypeCustomer
	}
	if u.Outcome == "" {
		u.Outcome = billing.UsageSucceeded
	}
	at := time.Now().UTC()
	if u.OccurredAt != nil {
		at = u.OccurredAt.UTC()
	}
	return billing.LeaseUsage{
		CustomerID: u.CustomerID, Invoker: u.Invoker, InvokerType: u.InvokerType, Currency: u.Currency,
		Source: u.Source, SourceID: u.SourceID, Outcome: u.Outcome, EventType: u.EventType,
		Dimensions: nonNilDims(u.Dimensions), Metadata: nonNilMeta(u.Metadata), Amount: u.Amount, OccurredAt: at,
	}, nil
}

func nonNilDims(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

func nonNilMeta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// normalizeGateAdmit checks what a gate decision needs of a request.
func normalizeGateAdmit(p *billing.AdmitParams) error {
	p.RequestID = strings.TrimSpace(p.RequestID)
	p.Currency = normalizeCurrency(p.Currency)
	switch {
	case p.RequestID == "":
		return invalidParam("request_id", "request_id is required")
	case p.CustomerID.IsZero():
		return invalidParam("customer_id", "customer_id is required")
	case p.Currency == "":
		return invalidParam("currency", "currency is required")
	case p.EstimatedAmount < 0:
		return invalidParam("estimated_amount", "estimated_amount must be nonnegative")
	case p.Usage != nil && p.EstimatedAmount != 0:
		return invalidParam("usage", "state usage or estimated_amount, not both")
	}
	if p.Usage != nil {
		p.Usage.EventType = strings.TrimSpace(p.Usage.EventType)
		if p.Usage.EventType == "" {
			return invalidParam("usage.event_type", "usage.event_type is required")
		}
		if p.Usage.Amount != nil && *p.Usage.Amount < 0 {
			return invalidParam("usage.amount", "usage.amount must be nonnegative")
		}
	}
	if p.InvokerType == "" {
		p.InvokerType = billing.InvokerTypeCustomer
	}
	return nil
}

// attrsKey names what policy resolution reads of a request: requests with
// equal keys share a limit set.
func attrsKey(p billing.AdmitParams) string {
	roles := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		roles[i] = r.String()
	}
	slices.Sort(roles)
	return strings.Join([]string{p.CustomerID.String(), p.Invoker, string(p.InvokerType), p.TrustLevel, strings.Join(roles, ","), p.Resource, p.Currency}, "\x00")
}

func leaseWant(p billing.AdmitParams) billing.LeaseWant {
	w := billing.LeaseWant{CustomerID: p.CustomerID, Invoker: p.Invoker, InvokerType: p.InvokerType, Roles: p.Roles, Currency: p.Currency, Usage: p.Usage}
	if w.Roles == nil {
		w.Roles = []uuid.UUID{}
	}
	if p.TrustLevel != "" {
		w.TrustLevel = &p.TrustLevel
	}
	if p.Resource != "" {
		w.Resource = &p.Resource
	}
	return w
}

// unavailable wraps a sync failure as ErrUnavailable.
func unavailable(cause error) error {
	if cause == nil || errors.Is(cause, ErrUnavailable) {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, cause)
}

func refusedTicket(g *Gate, p billing.AdmitParams, r *billing.Refusal) *Ticket {
	return &Ticket{gate: g, params: p, refusal: r}
}

func capacityHeld(retry int64) *billing.Refusal {
	return &billing.Refusal{DenyCode: billing.DenyCapacityHeld, RetryAfterSeconds: &retry}
}

func errNotPriced(eventType string) error {
	return &billing.StatusError{Status: http.StatusUnprocessableEntity, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodeUsageNotPriced,
		Message: fmt.Sprintf("the hold OpenRails issued does not price event type %q", eventType)}}
}

// waitRetry waits out a capacity_held refusal, or until ctx ends.
func waitRetry(ctx context.Context, r *billing.Refusal) bool {
	d := limits.SyncInterval
	if r != nil && r.RetryAfterSeconds != nil && *r.RetryAfterSeconds > 0 {
		d = time.Duration(*r.RetryAfterSeconds) * time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// admitRemote admits a request that declares an accrual rate as a request
// hold on the server, as Client.Admit does; capacity_held is waited out
// within ctx.
func (g *Gate) admitRemote(ctx context.Context, p billing.AdmitParams) (*Ticket, error) {
	for {
		verdicts, err := g.c.Admit(ctx, []billing.AdmitParams{p})
		if errors.Is(err, billing.ErrUnreachable) {
			return nil, unavailable(err)
		}
		if err != nil {
			return nil, err
		}
		if len(verdicts) != 1 {
			return nil, fmt.Errorf("%w: admit answered %d verdicts for one request", billing.ErrUnreachable, len(verdicts))
		}
		v := verdicts[0]
		if v.Admission == nil {
			if v.Error != nil {
				return nil, &billing.StatusError{Status: v.Status, ErrorDetails: *v.Error}
			}
			return nil, fmt.Errorf("%w: admit answered neither an admission nor an error", billing.ErrUnreachable)
		}
		if r := v.Admission.Refusal(); r != nil || !v.Allowed() {
			if r == nil {
				r = &billing.Refusal{DenyCode: "not_admitted"}
			}
			if r.DenyCode == billing.DenyCapacityHeld && waitRetry(ctx, r) {
				continue
			}
			return refusedTicket(g, p, r), nil
		}
		return &Ticket{gate: g, params: p}, nil
	}
}

func (g *Gate) captureRemote(ctx context.Context, t *Ticket, u billing.CaptureUsage) error {
	results, err := g.c.CaptureAdmissions(ctx, []billing.CaptureAdmissionParams{{RequestID: t.params.RequestID, Usage: u, Amount: statedAmount(t.params)}})
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("%w: capture answered %d results for one item", billing.ErrUnreachable, len(results))
	}
	return results[0].Err()
}

func (g *Gate) releaseRemote(ctx context.Context, t *Ticket) error {
	results, err := g.c.ReleaseAdmissions(ctx, []string{t.params.RequestID})
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return fmt.Errorf("%w: release answered %d results for one item", billing.ErrUnreachable, len(results))
	}
	return results[0].Err()
}
