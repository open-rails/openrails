package spendgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
)

var (
	ErrNotFound          = errors.New("admission not found")
	ErrExpired           = errors.New("admission deadline has passed")
	ErrDeadlineRequired  = errors.New("admission deadline is required")
	ErrCaptured          = errors.New("admission is already captured")
	ErrDeadlineShortened = errors.New("extension cannot shorten the deadline")
)

// MaxWindow and MaxHold bound what a hold can still matter to, so its
// monthly partition can be dropped by the calendar alone.
const (
	MaxWindow = retention.AdmissionMaxWindow
	MaxHold   = retention.AdmissionMaxHold
)

func deadlineTooFar() error {
	return &ValidationError{Param: "expires_at", Message: fmt.Sprintf("expires_at must be within %d days of the admission", int(MaxHold/retention.Day))}
}

// retainedSince bounds a lookup by request id to the admissions still kept.
func retainedSince(now time.Time) time.Time { return retention.AdmissionsDropBefore(now) }

type Conflict struct{ Field string }

func (e *Conflict) Error() string { return "admission request id reused with changed " + e.Field }

// ValidationError identifies a malformed command before any money lock or write.
type ValidationError struct{ Param, Message string }

func (e *ValidationError) Error() string { return e.Message }

// ValidateRequest is shared by the HTTP boundary and transaction-owning callers.
func ValidateRequest(requestID string, amount, prospectiveRate int64) error {
	if id := strings.TrimSpace(requestID); id == "" || len(id) > 255 {
		return &ValidationError{Param: "request_id", Message: "request_id must contain 1 to 255 bytes"}
	}
	if amount < 0 {
		return &ValidationError{Param: "estimated_amount", Message: "estimated_amount must be nonnegative"}
	}
	if prospectiveRate < 0 {
		return &ValidationError{Param: "accrual_rate_delta_per_hour", Message: "accrual_rate_delta_per_hour must be nonnegative"}
	}
	return nil
}

// Terms preserve caller-supplied dimensions; resolved policy never rewrites them.
type Terms struct {
	Invoker                 string   `json:"invoker"`
	InvokerType             string   `json:"invoker_type"`
	TrustLevel              string   `json:"trust_level"`
	Roles                   []string `json:"roles"`
	Resource                string   `json:"resource"`
	Source                  string   `json:"source"`
	AccrualRateDeltaPerHour int64    `json:"accrual_rate_delta_per_hour"`
}

func (t Terms) normalized() Terms {
	t.Invoker = strings.TrimSpace(t.Invoker)
	t.InvokerType = strings.TrimSpace(t.InvokerType)
	t.TrustLevel = strings.TrimSpace(t.TrustLevel)
	t.Resource = strings.TrimSpace(t.Resource)
	t.Source = strings.TrimSpace(t.Source)
	t.Roles = append([]string{}, t.Roles...)
	slices.Sort(t.Roles)
	t.Roles = slices.Compact(t.Roles)
	return t
}

func OriginalTerms(row gen.BillingLimitHold) (Terms, error) {
	var terms Terms
	if err := json.Unmarshal(row.Terms, &terms); err != nil {
		return terms, fmt.Errorf("decode admission terms: %w", err)
	}
	return terms.normalized(), nil
}

type Gate struct {
	db  *db.DB
	now func() time.Time
}

func New(d *db.DB) *Gate { return &Gate{db: d, now: time.Now} }
func (g *Gate) SetClock(now func() time.Time) {
	if now != nil {
		g.now = now
	}
}
func (g *Gate) Now() time.Time { return g.now().UTC().Truncate(time.Microsecond) }

type Decision struct {
	Allowed bool
	// EstimatedAmount is what the admission holds: the stored one on a replay.
	EstimatedAmount int64
	BlockedBalance  bool
	BlockedWindow   *Window
	// BlockedLimit is the declared limit that refused, and when it resets.
	BlockedLimit    *LimitBlock
	RetryAfter      time.Duration
	Replayed        bool
	ExpiresAt       *time.Time
	State           string
	AvailableAmount int64
}
type AdmitInput struct {
	Customer  uuid.UUID
	Currency  string
	RequestID string
	Cost      int64
	ExpiresAt time.Time
	Terms     Terms
	// AccountBalance excludes all existing request/provider reservations.
	AccountBalance int64
	CreditLimit    int64
	Policy         Policy
	Request        Request
	// EstimateUsage is the usage estimate Cost was priced from, and RateCards
	// the cards that priced it; both nil for a host-stated estimate.
	EstimateUsage []byte
	RateCards     []byte
	// Limits are the declared limits the request counts toward.
	Limits []limits.Claim
}

// LimitBlock is a declared limit's refusal.
type LimitBlock struct {
	Key      string
	ResetsAt *time.Time
}

// Admit runs inside the transaction that owns the payer money lock.
func (g *Gate) Admit(ctx context.Context, q *gen.Queries, in AdmitInput) (Decision, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Decision{}, err
	}
	if in.Customer == uuid.Nil {
		return Decision{}, &ValidationError{Param: "customer_id", Message: "customer_id required"}
	}
	if err := ValidateRequest(in.RequestID, in.Cost, in.Terms.AccrualRateDeltaPerHour); err != nil {
		return Decision{}, err
	}
	if in.CreditLimit < 0 {
		return Decision{}, &ValidationError{Param: "credit_limit", Message: "credit_limit must be nonnegative"}
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	in.Terms = in.Terms.normalized()
	now := g.Now()
	// The request id is claimed under this lock: no unique key spans partitions.
	if err := q.LockHoldRequest(ctx, gen.LockHoldRequestParams{MerchantID: mid.UUID(), RequestID: in.RequestID}); err != nil {
		return Decision{}, err
	}
	if row, err := q.GetRequestHold(ctx, gen.GetRequestHoldParams{MerchantID: mid.UUID(), RequestID: in.RequestID, CreatedSince: retainedSince(now)}); err == nil {
		return g.replay(row, in)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, err
	}
	expiry := deadline(in.ExpiresAt)
	holds := in.Cost > 0
	for _, c := range in.Limits {
		holds = holds || c.Cost > 0
	}
	if holds && expiry == nil {
		return Decision{}, ErrDeadlineRequired
	}
	if expiry != nil && !expiry.After(now) {
		return Decision{}, ErrExpired
	}
	if expiry != nil && expiry.After(now.Add(MaxHold)) {
		return Decision{}, deadlineTooFar()
	}
	capacity := in.AccountBalance
	if capacity > math.MaxInt64-in.CreditLimit {
		capacity = math.MaxInt64
	} else {
		capacity += in.CreditLimit
	}
	if in.Cost > capacity {
		return Decision{BlockedBalance: true, EstimatedAmount: in.Cost}, nil
	}
	// Spend windows are currency limits on their hashed anchors; they and the
	// declared limits are taken together, all or none.
	windows := in.Policy.EffectiveWindows(in.Request)
	claims, err := SpendClaims(mid.UUID(), in.Customer, in.Currency, in.Policy, in.Request, in.Cost, now)
	if err != nil {
		return Decision{}, err
	}
	claims = append(claims, in.Limits...)
	taken, err := limits.Holder{Q: q, MerchantID: mid.UUID(), CustomerID: in.Customer, Now: now}.Take(ctx, claims)
	if err != nil {
		return Decision{}, err
	}
	if taken.Refused >= 0 {
		if taken.Refused < len(windows) {
			blocked := windows[taken.Refused].Window
			return Decision{BlockedWindow: &blocked, RetryAfter: claims[taken.Refused].Window.EndsAt.Sub(now), EstimatedAmount: in.Cost}, nil
		}
		return Decision{BlockedLimit: &LimitBlock{Key: claims[taken.Refused].Key, ResetsAt: taken.ResetsAt}, EstimatedAmount: in.Cost}, nil
	}
	body, err := json.Marshal(in.Terms)
	if err != nil {
		return Decision{}, err
	}
	row, err := q.InsertRequestHold(ctx, gen.InsertRequestHoldParams{
		MerchantID: mid.UUID(), CreatedAt: now, CustomerID: in.Customer, Currency: in.Currency, RequestID: in.RequestID,
		EstimatedAmount: in.Cost, AvailableAmount: capacity - in.Cost, Terms: body,
		EstimateUsage: in.EstimateUsage, RateCards: in.RateCards, RequestedExpiresAt: expiry,
	})
	if err != nil {
		return Decision{}, err
	}
	part := func(key, unit string, w limits.Window, held int64) error {
		return q.InsertHoldPart(ctx, gen.InsertHoldPartParams{MerchantID: mid.UUID(), HoldID: row.ID, HoldCreatedAt: row.CreatedAt, CustomerID: in.Customer,
			LimitKey: key, Unit: unit, WindowStartsAt: w.StartsAt, WindowEndsAt: w.EndsAt, Held: held})
	}
	if in.Cost > 0 {
		if err := part(catalog.BalanceLimit, in.Currency, limits.Window{StartsAt: limits.Epoch}, in.Cost); err != nil {
			return Decision{}, err
		}
	}
	// Every limit keeps its window even when it holds nothing: the capture
	// settles there.
	for i, c := range claims {
		if err := part(c.Key, c.Unit, c.Window, taken.Held[i]); err != nil {
			return Decision{}, err
		}
	}
	// A hold takes from what the customer may spend: one that crosses a line
	// of its balance watch queues a look.
	if err := ledger.WatchChange(ctx, q, mid.UUID(), in.Customer, in.Currency, false, now); err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: true, EstimatedAmount: in.Cost, ExpiresAt: row.ExpiresAt, State: "open", AvailableAmount: capacity - in.Cost}, nil
}

// CheckIdentity refuses key reuse before current policy evaluation can mask a conflict.
func (g *Gate) CheckIdentity(ctx context.Context, q *gen.Queries, in AdmitInput) (*Decision, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := q.GetRequestHold(ctx, gen.GetRequestHoldParams{MerchantID: mid.UUID(), RequestID: in.RequestID, CreatedSince: retainedSince(g.Now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decision, err := g.replay(row, in)
	return &decision, err
}

func (g *Gate) replay(row gen.BillingLimitHold, in AdmitInput) (Decision, error) {
	field := ""
	estimated := len(row.EstimateUsage) > 0 || len(in.EstimateUsage) > 0
	switch {
	case row.CustomerID != in.Customer:
		field = "payer"
	case row.Currency != in.Currency:
		field = "currency"
	// A usage estimate is the term: its price is what admitting it answered.
	case estimated && !sameUsageEstimate(row.EstimateUsage, in.EstimateUsage):
		field = "usage"
	case !estimated && amount(row.EstimatedAmount) != in.Cost:
		field = "estimated_amount"
	case !sameDeadline(row.RequestedExpiresAt, deadline(in.ExpiresAt)):
		field = "expires_at"
	default:
		original, err := OriginalTerms(row)
		if err != nil {
			return Decision{}, err
		}
		a, err := json.Marshal(original)
		if err != nil {
			return Decision{}, err
		}
		b, err := json.Marshal(in.Terms.normalized())
		if err != nil {
			return Decision{}, err
		}
		if !bytes.Equal(a, b) {
			field = "terms"
		}
	}
	if field != "" {
		return Decision{}, &Conflict{Field: field}
	}
	state := row.State
	if state == "open" && row.ExpiresAt != nil && !row.ExpiresAt.After(g.Now()) {
		state = "expired"
	}
	return Decision{Allowed: true, Replayed: true, EstimatedAmount: amount(row.EstimatedAmount), ExpiresAt: row.ExpiresAt, State: state, AvailableAmount: amount(row.AvailableAmount)}, nil
}

func amount(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// sameUsageEstimate compares stored jsonb by value while keeping dimensions
// as int64: decoding them into any would round quantities above 2^53.
func sameUsageEstimate(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y billing.UsageEstimate
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, err1 := json.Marshal(x)
	jb, err2 := json.Marshal(y)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

func deadline(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC().Truncate(time.Microsecond)
	return &t
}
func sameDeadline(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (g *Gate) Get(ctx context.Context, requestID string) (gen.BillingLimitHold, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingLimitHold{}, err
	}
	row, err := g.db.Gen(ctx).GetRequestHold(ctx, gen.GetRequestHoldParams{MerchantID: mid.UUID(), RequestID: requestID, CreatedSince: retainedSince(g.Now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

// WithOperation locks the customer before the hold; its ownership is
// immutable.
func (g *Gate) WithOperation(ctx context.Context, requestID string, fn func(context.Context, *db.DB, gen.BillingLimitHold) error) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return g.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetRequestHold(ctx, gen.GetRequestHoldParams{MerchantID: mid.UUID(), RequestID: requestID, CreatedSince: retainedSince(g.Now())})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: row.CustomerID}); err != nil {
			return err
		}
		row, err = q.LockHold(ctx, gen.LockHoldParams{MerchantID: mid.UUID(), ID: row.ID, CreatedAt: row.CreatedAt})
		if err != nil {
			return err
		}
		return fn(ctx, g.db.NewWithPgxTx(tx), row)
	})
}

func (g *Gate) Release(ctx context.Context, requestID string) error {
	return g.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.BillingLimitHold) error {
		if row.State == "captured" {
			return ErrCaptured
		}
		if row.State == "released" {
			return nil
		}
		_, err := d.Gen(ctx).ReleaseRequestHold(ctx, gen.ReleaseRequestHoldParams{MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, AsOf: g.Now()})
		return err
	})
}

func (g *Gate) Extend(ctx context.Context, requestID string, until time.Time) error {
	if until.IsZero() || !until.After(g.Now()) {
		return ErrExpired
	}
	return g.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.BillingLimitHold) error {
		if row.State != "open" {
			return ErrNotFound
		}
		if row.ExpiresAt != nil && !row.ExpiresAt.After(g.Now()) {
			return ErrExpired
		}
		if row.ExpiresAt != nil && until.Before(*row.ExpiresAt) {
			return ErrDeadlineShortened
		}
		if until.After(row.CreatedAt.Add(MaxHold)) {
			return deadlineTooFar()
		}
		_, err := d.Gen(ctx).ExtendRequestHold(ctx, gen.ExtendRequestHoldParams{MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, AsOf: g.Now(), ExpiresAt: until.UTC()})
		return err
	})
}

// SpendClaims are a request's spend windows as currency limits on their
// hashed anchors, each claiming cost, in policy order.
func SpendClaims(merchantID, customer uuid.UUID, currency string, policy Policy, req Request, cost int64, now time.Time) ([]limits.Claim, error) {
	windows := policy.EffectiveWindows(req)
	claims := make([]limits.Claim, 0, len(windows))
	for _, w := range windows {
		key, start, end, err := windowPeriod(merchantID, customer, currency, w, now)
		if err != nil {
			return nil, err
		}
		claims = append(claims, limits.Claim{Key: limits.SpendKeyPrefix + key, Unit: currency,
			Reset:  catalog.LimitReset{Kind: catalog.ResetFixed, WindowSeconds: int(w.Duration / time.Second)},
			Window: limits.Window{StartsAt: start, EndsAt: &end, Open: true}, Capacity: w.Limit, Cost: cost, Overrun: catalog.OverrunRequireEstimate})
	}
	return claims, nil
}

func fixedOffsetMs(prefix string, durMs int64) int64 {
	if durMs <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix))
	offset, _ := safecast.Convert[int64](h.Sum64() % uint64(durMs))
	return offset
}

func windowPeriod(mid, payer uuid.UUID, currency string, w resolvedWindow, now time.Time) (string, time.Time, time.Time, error) {
	// A window longer than MaxWindow would count admissions already dropped.
	if w.Duration < time.Millisecond || w.Duration > MaxWindow || w.Limit < 0 {
		return "", time.Time{}, time.Time{}, fmt.Errorf("invalid spend window")
	}
	key := w.identity(fmt.Sprintf("%s/%s/%s", mid, payer, currency))
	duration := w.Duration.Milliseconds()
	offset := fixedOffsetMs(key, duration)
	bucket := (now.UnixMilli() - offset) / duration
	start := time.UnixMilli(offset + bucket*duration).UTC()
	return key, start, start.Add(w.Duration), nil
}
