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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrNotFound          = errors.New("admission operation not found")
	ErrExpired           = errors.New("admission deadline has passed")
	ErrDeadlineRequired  = errors.New("admission deadline is required")
	ErrCaptured          = errors.New("admission operation is already captured")
	ErrDeadlineShortened = errors.New("extension cannot shorten the deadline")
)

type Conflict struct{ Field string }

func (e *Conflict) Error() string { return "admission request id reused with changed " + e.Field }

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

func OriginalTerms(row gen.OpenrailsAdmissionOperation) (Terms, error) {
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
	Allowed         bool
	BlockedBalance  bool
	BlockedWindow   *Window
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
}

// Admit runs inside the transaction that owns the payer money lock.
func (g *Gate) Admit(ctx context.Context, q *gen.Queries, in AdmitInput) (Decision, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Decision{}, err
	}
	if in.Customer == uuid.Nil || strings.TrimSpace(in.RequestID) == "" || len(in.RequestID) > 255 {
		return Decision{}, fmt.Errorf("payer and request_id (at most 255 bytes) are required")
	}
	if in.Cost < 0 || in.CreditLimit < 0 || in.Terms.AccrualRateDeltaPerHour < 0 {
		return Decision{}, fmt.Errorf("admission amounts must be nonnegative")
	}
	in.Terms = in.Terms.normalized()
	if row, err := q.GetAdmissionOperation(ctx, gen.GetAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: in.RequestID}); err == nil {
		return g.replay(row, in)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, err
	}
	now := g.Now()
	expiry := deadline(in.ExpiresAt)
	if in.Cost > 0 && expiry == nil {
		return Decision{}, ErrDeadlineRequired
	}
	if expiry != nil && !expiry.After(now) {
		return Decision{}, ErrExpired
	}
	capacity := in.AccountBalance
	if capacity > math.MaxInt64-in.CreditLimit {
		capacity = math.MaxInt64
	} else {
		capacity += in.CreditLimit
	}
	if in.Cost > capacity {
		return Decision{BlockedBalance: true}, nil
	}
	windows := in.Policy.EffectiveWindows(in.Request)
	keys := make([]string, 0, len(windows))
	for _, w := range windows {
		key, start, end, err := windowPeriod(mid.UUID(), in.Customer, in.Currency, w, now)
		if err != nil {
			return Decision{}, err
		}
		usage, err := q.AdmissionWindowUsage(ctx, gen.AdmissionWindowUsageParams{
			MerchantID: mid.UUID(), PayerID: in.Customer, Currency: in.Currency,
			WindowKey: key, WindowStart: start, WindowEnd: end, AsOf: now,
		})
		if err != nil {
			return Decision{}, err
		}
		if in.Cost > w.Limit || usage.Used > w.Limit-in.Cost {
			blocked := w.Window
			return Decision{BlockedWindow: &blocked, RetryAfter: end.Sub(now)}, nil
		}
		keys = append(keys, key)
	}
	body, err := json.Marshal(in.Terms)
	if err != nil {
		return Decision{}, err
	}
	row, err := q.InsertAdmissionOperation(ctx, gen.InsertAdmissionOperationParams{
		MerchantID: mid.UUID(), RequestID: in.RequestID, PayerID: in.Customer,
		Currency: in.Currency, EstimatedAmount: in.Cost, Terms: body,
		AvailableAmount:    capacity - in.Cost,
		RequestedExpiresAt: expiry, AdmittedAt: now, WindowKeys: keys,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = q.GetAdmissionOperation(ctx, gen.GetAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: in.RequestID})
		if err != nil {
			return Decision{}, err
		}
		return g.replay(row, in)
	}
	if err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: true, ExpiresAt: row.ExpiresAt, State: "open", AvailableAmount: row.AvailableAmount}, nil
}

// CheckIdentity refuses key reuse before current policy evaluation can mask a conflict.
func (g *Gate) CheckIdentity(ctx context.Context, q *gen.Queries, in AdmitInput) (*Decision, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := q.GetAdmissionOperation(ctx, gen.GetAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: in.RequestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decision, err := g.replay(row, in)
	return &decision, err
}

func (g *Gate) replay(row gen.OpenrailsAdmissionOperation, in AdmitInput) (Decision, error) {
	field := ""
	switch {
	case row.PayerID != in.Customer:
		field = "payer"
	case row.Currency != in.Currency:
		field = "currency"
	case row.EstimatedAmount != in.Cost:
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
	return Decision{Allowed: true, Replayed: true, ExpiresAt: row.ExpiresAt, State: state, AvailableAmount: row.AvailableAmount}, nil
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

func (g *Gate) Get(ctx context.Context, requestID string) (gen.OpenrailsAdmissionOperation, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsAdmissionOperation{}, err
	}
	row, err := g.db.Gen(ctx).GetAdmissionOperation(ctx, gen.GetAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

// WithOperation locks payer before operation; original ownership is immutable.
func (g *Gate) WithOperation(ctx context.Context, requestID string, fn func(context.Context, *db.DB, gen.OpenrailsAdmissionOperation) error) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return g.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetAdmissionOperation(ctx, gen.GetAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: requestID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: row.PayerID}); err != nil {
			return err
		}
		row, err = q.LockAdmissionOperation(ctx, gen.LockAdmissionOperationParams{MerchantID: mid.UUID(), RequestID: requestID})
		if err != nil {
			return err
		}
		return fn(ctx, g.db.NewWithPgxTx(tx), row)
	})
}

func (g *Gate) Release(ctx context.Context, requestID string) error {
	return g.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.OpenrailsAdmissionOperation) error {
		if row.State == "captured" {
			return ErrCaptured
		}
		if row.State == "released" {
			return nil
		}
		_, err := d.Gen(ctx).ReleaseAdmissionOperation(ctx, gen.ReleaseAdmissionOperationParams{MerchantID: row.MerchantID, RequestID: row.RequestID, AsOf: g.Now()})
		return err
	})
}

func (g *Gate) Extend(ctx context.Context, requestID string, until time.Time) error {
	if until.IsZero() || !until.After(g.Now()) {
		return ErrExpired
	}
	return g.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.OpenrailsAdmissionOperation) error {
		if row.State != "open" {
			return ErrNotFound
		}
		if row.ExpiresAt != nil && !row.ExpiresAt.After(g.Now()) {
			return ErrExpired
		}
		if row.ExpiresAt != nil && until.Before(*row.ExpiresAt) {
			return ErrDeadlineShortened
		}
		_, err := d.Gen(ctx).ExtendAdmissionOperation(ctx, gen.ExtendAdmissionOperationParams{MerchantID: row.MerchantID, RequestID: row.RequestID, AsOf: g.Now(), ExpiresAt: until.UTC()})
		return err
	})
}

func fixedOffsetMs(prefix string, durMs int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix))
	return int64(h.Sum64() % uint64(durMs))
}

func windowPeriod(mid, payer uuid.UUID, currency string, w resolvedWindow, now time.Time) (string, time.Time, time.Time, error) {
	if w.Duration < time.Millisecond || w.Limit < 0 {
		return "", time.Time{}, time.Time{}, fmt.Errorf("invalid spend window")
	}
	key := w.identity(fmt.Sprintf("%s/%s/%s", mid, payer, currency))
	duration := w.Duration.Milliseconds()
	offset := fixedOffsetMs(key, duration)
	bucket := (now.UnixMilli() - offset) / duration
	start := time.UnixMilli(offset + bucket*duration).UTC()
	return key, start, start.Add(w.Duration), nil
}
