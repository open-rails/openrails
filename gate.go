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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Gate admits requests against a merchant's usage limits in the host's
// process. Each sync answers views: where each customer the gate serves
// stands on its limits, which reserve nothing. Every decision takes the
// request's cost from the view, less what the gate has in flight, with no
// I/O; nothing is decided or held on the server. Overshoot is bounded by
// what other gates admit within about four syncs, plus one request per
// gate. While OpenRails is unreachable the gate keeps
// deciding on its views, spending at most its share of each. The gate syncs
// while it is active, carrying the usage its tickets captured, which the
// server prices exactly. It works over the remote Client and the embedded
// engine's alike. Use one gate per merchant handle and process.
type Gate struct {
	c *Client
	g *gateState

	mu     sync.Mutex
	closed bool
	// noStore is the server's event_store_required: every later Admit fails
	// with it.
	noStore error
}

// GateDeps is what a Gate keeps beyond its process. Nothing tunes timing,
// sizes or tolerance: those follow from traffic.
type GateDeps struct {
	// Outbox keeps captured usage durably until OpenRails settles it, so a
	// restarted process resends it; without it a crash loses unsent usage,
	// which is never billed.
	Outbox UsageOutbox
}

// UsageOutbox stores a gate's captured usage until OpenRails settles it. It
// must be safe for concurrent use.
//
// Put stores all of its items or none: an error means it stored nothing. A
// Capture or Record whose Put fails fails, and its usage is never billed; an
// outbox that stored an item and still returned an error has it refused as a
// changed item when it is resent, never billed twice. Pending is read when a
// gate starts and resent against the original leases. Ack follows
// settlement.
type UsageOutbox interface {
	Put(ctx context.Context, items []billing.OutboxItem) error
	Pending(ctx context.Context) ([]billing.OutboxItem, error)
	Ack(ctx context.Context, items []billing.LeaseUsageRef) error
}

// ErrUnavailable is a decision the gate could not make because OpenRails
// could not be reached and no view (or the gate's share of it) covered it.
// It is also billing.ErrUnreachable.
var ErrUnavailable = fmt.Errorf("openrails: gate: OpenRails is unavailable: %w", billing.ErrUnreachable)

// errGateClosed is a call on a closed gate.
var errGateClosed = errors.New("openrails: gate: closed")

// newLeaseID is a gate's lease id: a UUIDv7, as the server requires.
func newLeaseID() billing.LeaseID { return billing.LeaseID(uuid.Must(uuid.NewV7())) }

// NewGate starts a gate over c, a remote or embedded merchant handle acting
// as itself: a gate serves every request, so a handle made with ActingFor is
// refused. Its lease is created on the first sync. An engine without the
// event store (Deps.ClickHouse) is billing.ErrEventStoreRequired; over HTTP
// the first sync's event_store_required fails every Admit.
func NewGate(c *Client, deps GateDeps) (*Gate, error) {
	switch {
	case c == nil:
		return nil, fmt.Errorf("openrails: NewGate needs a merchant handle")
	case c.engine != nil && c.engine.App.Runtime.EventStore == nil:
		return nil, billing.ErrEventStoreRequired
	case c.invoker != nil:
		return nil, fmt.Errorf("openrails: NewGate needs the merchant's own handle, not one made with ActingFor")
	}
	g := &Gate{c: c, g: newGateState(c, deps.Outbox)}
	if deps.Outbox != nil {
		// A restarted process resends its outbox at once.
		g.g.ensureStarted()
	}
	return g, nil
}

// keepNoStore keeps err when it is event_store_required, and returns it.
func (g *Gate) keepNoStore(err error) error {
	if errors.Is(err, billing.ErrEventStoreRequired) {
		g.mu.Lock()
		if g.noStore == nil {
			g.noStore = err
		}
		g.mu.Unlock()
	}
	return err
}

// keptNoStore is the event_store_required a sync answered, or nil.
func (g *Gate) keptNoStore() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.noStore
}

func (g *Gate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// Admit decides one request on the gate's view. A refusal is a verdict
// (Ticket.Refusal), not an error: limit_reached names the limit and when it
// resets, and money refusals keep their deny codes. The error is for bad
// input, ctx, usage_not_priced (no rate card prices the request's usage), or
// ErrUnavailable. A ticket lives while the gate does.
func (g *Gate) Admit(ctx context.Context, p billing.AdmitParams) (*Ticket, error) {
	if g.isClosed() {
		return nil, errGateClosed
	}
	if err := g.keptNoStore(); err != nil {
		return nil, err
	}
	if err := normalizeGateAdmit(&p); err != nil {
		return nil, err
	}
	t, err := g.g.admit(ctx, g, p)
	return t, g.keepNoStore(err)
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

// Record reports usage no ticket admitted, sent on the next sync. It counts
// toward the customer's limits and is billed as the server prices it. Every
// Record that returns nil posts once, and one that returns an error stored
// nothing, so a retry posts once.
func (g *Gate) Record(ctx context.Context, u billing.UsageParams) error {
	if g.isClosed() {
		return errGateClosed
	}
	item, err := recordItem(u)
	if err != nil {
		return err
	}
	return g.g.record(ctx, item)
}

// ListCustomerLimits is where the customer stands on each limit, for
// display: its views' lines with what the gate has in flight added, or a
// read when the gate has no view of it. It never adds spendable capacity.
func (g *Gate) ListCustomerLimits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	if customer.IsZero() {
		return nil, invalidParam("customer_id", "customer_id is required")
	}
	return g.g.limits(ctx, customer)
}

// Flush syncs now and returns once every usage item captured before the call
// is settled (and acked in the Outbox), or with ErrUnavailable, the sync's
// refusal or ctx's error; under a ctx with no deadline it gives up after a
// bound with ErrUnavailable. It returns the errors of items OpenRails
// refused since the last Flush; those posted nothing.
func (g *Gate) Flush(ctx context.Context) error {
	return g.g.flush(ctx, false)
}

// Close sends pending usage and ends the gate's lease
// (SyncLeasesParams.Close), which clears its in-flight report, then stops syncing, within ctx (or a bound when ctx has
// no deadline). A sync that cannot reach OpenRails is retried until then;
// usage still unsent at the end is named in the error, and without an Outbox
// it is lost when the process exits. Close after the gate's tickets are
// captured or released: a ticket captured after Close is refused.
func (g *Gate) Close(ctx context.Context) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	ctx, cancel := boundContext(ctx)
	defer cancel()
	return g.g.close(ctx)
}

// boundContext bounds a ctx that has no deadline by gateFlushBound.
func boundContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, gateFlushBound, unavailable(errors.New("usage did not settle within the gate's bound")))
}

// waitErr is why a wait under ctx ended: the bound's ErrUnavailable, or
// ctx's own error.
func waitErr(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrUnavailable) {
		return cause
	}
	return ctx.Err()
}

// Ticket is one decision of a gate. An allowed ticket is captured with the
// request's usage or released; one that is neither stays open while the gate
// lives.
type Ticket struct {
	gate    *Gate
	params  billing.AdmitParams
	refusal *billing.Refusal

	mu    sync.Mutex
	state ticketState

	// view and takes are what the decision took of the gate's view.
	view       *custView
	takes      []take
	admittedAt time.Time
	// build is the gate's sync count when the ticket was admitted: it is
	// reported in flight once a sync built after it has passed.
	build int64
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
// the request id. A dynamic card charges the request's stated amount. A
// capture that fails leaves the ticket open, to capture again or release.
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
		return &billing.StatusError{Status: http.StatusConflict, ErrorDetails: billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeResourceConflict, Message: "the ticket was already captured or released"}}
	}
	t.state = ticketCaptured
	t.mu.Unlock()
	if err := t.gate.g.capture(ctx, t, u); err != nil {
		t.mu.Lock()
		t.state = ticketOpen
		t.mu.Unlock()
		return err
	}
	return nil
}

// Release gives the request's take back when its work failed; it is a no-op
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
	t.gate.g.release(t)
	return nil
}

// captureUsage fills a capture's defaults and checks it against the
// request.
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

// captureItem is a ticket's usage as a lease reports it, numbered later: the
// invoker attributes it was admitted with, and when.
func captureItem(t *Ticket, u billing.CaptureUsage, now time.Time) billing.LeaseUsage {
	p := t.params
	item := billing.LeaseUsage{
		CustomerID: p.CustomerID, Invoker: p.Invoker, InvokerType: p.InvokerType, Roles: p.Roles, Currency: p.Currency,
		Source: u.Source, SourceID: u.SourceID, Outcome: billing.UsageSucceeded, EventType: u.EventType,
		Dimensions: nonNilDims(u.Dimensions), Metadata: nonNilMeta(u.Metadata), Amount: statedAmount(p), OccurredAt: now.UTC(),
	}
	if p.TrustLevel != "" {
		item.TrustLevel = &p.TrustLevel
	}
	if p.Resource != "" {
		item.Resource = &p.Resource
	}
	at := t.admittedAt.UTC()
	item.AdmittedAt = &at
	return item
}

func recordItem(u billing.UsageParams) (billing.LeaseUsage, error) {
	u.Currency = catalog.Currency(normalizeCurrency(string(u.Currency)))
	u.EventType = strings.TrimSpace(u.EventType)
	switch {
	case u.CustomerID.IsZero():
		return billing.LeaseUsage{}, invalidParam("customer_id", "customer_id is required")
	case u.Currency == "":
		return billing.LeaseUsage{}, invalidParam("currency", "currency is required")
	case moneyutil.ValidateBalanceCurrency(string(u.Currency)) != nil:
		return billing.LeaseUsage{}, currencyUnsupported()
	case !validInvokerType(u.InvokerType):
		return billing.LeaseUsage{}, invalidParam("invoker_type", `invoker_type must be "customer" or "delegated"`)
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
	p.Currency = catalog.Currency(normalizeCurrency(string(p.Currency)))
	switch {
	case p.RequestID == "" || len(p.RequestID) > 255:
		return invalidParam("request_id", "request_id must contain 1 to 255 bytes")
	case p.CustomerID.IsZero():
		return invalidParam("customer_id", "customer_id is required")
	case p.Currency == "":
		return invalidParam("currency", "currency is required")
	case moneyutil.ValidateBalanceCurrency(string(p.Currency)) != nil:
		return currencyUnsupported()
	case !validInvokerType(p.InvokerType):
		return invalidParam("invoker_type", `invoker_type must be "customer" or "delegated"`)
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
	p.TrustLevel, p.Resource = strings.TrimSpace(p.TrustLevel), strings.TrimSpace(p.Resource)
	return nil
}

func validInvokerType(t billing.InvokerType) bool {
	return t == "" || t == billing.InvokerTypeCustomer || t == billing.InvokerTypeDelegated
}

func currencyUnsupported() error {
	param := "currency"
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest,
		Code: billing.CodeCurrencyUnsupported, Message: "currency is not supported", Param: &param}}
}

// attrsKey names what policy resolution reads of a request: requests with
// equal keys claim the same limit set.
func attrsKey(p billing.AdmitParams) string {
	roles := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		roles[i] = r.String()
	}
	slices.Sort(roles)
	return strings.Join([]string{p.CustomerID.String(), p.Invoker, string(p.InvokerType), p.TrustLevel, strings.Join(roles, ","), p.Resource, string(p.Currency), eventTypeOf(p)}, "\x00")
}

func eventTypeOf(p billing.AdmitParams) string {
	if p.Usage == nil {
		return ""
	}
	return p.Usage.EventType
}

// viewParams asks for the view of a request's customer and attributes.
func viewParams(p billing.AdmitParams) billing.ViewParams {
	v := billing.ViewParams{CustomerID: p.CustomerID, Invoker: p.Invoker, InvokerType: p.InvokerType, Roles: p.Roles, Currency: p.Currency}
	if v.Roles == nil {
		v.Roles = []uuid.UUID{}
	}
	if p.TrustLevel != "" {
		v.TrustLevel = &p.TrustLevel
	}
	if p.Resource != "" {
		v.Resource = &p.Resource
	}
	if et := eventTypeOf(p); et != "" {
		v.EventType = &et
	}
	return v
}

// unreachable reports a call failure the gate takes as OpenRails
// unreachable: a transport failure, 5xx or 429.
func unreachable(err error) bool {
	var se *billing.StatusError
	return err != nil && (!errors.As(err, &se) || overloaded(se.Status))
}

// overloaded reports a status the gate takes as OpenRails unreachable.
func overloaded(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

// unavailable wraps a sync failure as ErrUnavailable.
func unavailable(cause error) error {
	switch {
	case cause == nil:
		return ErrUnavailable
	case errors.Is(cause, ErrUnavailable):
		return cause
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, cause)
}

func refusedTicket(g *Gate, p billing.AdmitParams, r *billing.Refusal) *Ticket {
	out := *r
	return &Ticket{gate: g, params: p, refusal: &out}
}

// normalizeCurrency is the server's spelling of a currency or unit code, so
// the gate's lines match its own: a qualified unit keeps its exact spelling.
func normalizeCurrency(currency string) string {
	currency = strings.TrimSpace(currency)
	if strings.ContainsAny(currency, ":/") {
		return currency
	}
	return strings.ToUpper(currency)
}

// gateTimes are the gate's timing constants; none is a setting.
const (
	// syncTimeout bounds one SyncLeases call, well inside the lease's life.
	syncTimeout = limits.LeaseLifetime / 2
	// gateFlushBound bounds Flush and Close under a context with no deadline.
	gateFlushBound = 3 * limits.LeaseLifetime
)
