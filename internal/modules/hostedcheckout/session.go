// Package hostedcheckout stores hosted checkout sessions (#1124): one buyer,
// one offer, addressed by a session id that is the only credential. A session
// is immutable after mint except for its payment attempt and the engine
// checkout session that attempt created.
package hostedcheckout

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// SessionTTL is how long a minted session can be paid.
	SessionTTL = 30 * time.Minute
	// ReconciliationWindow keeps an expired session readable, so a late
	// provider return still learns its outcome.
	ReconciliationWindow = 24 * time.Hour
	// MaxAttempts caps the payment attempts of one session.
	MaxAttempts = 10

	idPrefix = "ocs_"
	idBytes  = 32
)

var (
	ErrNotFound  = apperr.New(http.StatusNotFound, "checkout_session_not_found", "Checkout session not found.")
	ErrExpired   = apperr.New(http.StatusGone, "checkout_session_expired", "Checkout session expired.")
	ErrForbidden = apperr.New(http.StatusForbidden, "checkout_session_unavailable", "Checkout session is not available.")
	ErrBusy      = apperr.New(http.StatusConflict, "checkout_payment_in_progress", "A payment is already being processed.")
	ErrInvalid   = apperr.New(http.StatusUnprocessableEntity, "checkout_request_invalid", "Checkout request is invalid.")
)

// Option is one payment option as minted: the browser-facing rail plus the
// engine selector and PSP it is bound to. Only the rail leaves the server.
type Option struct {
	billing.HostedCheckoutRail
	Selector string `json:"selector"`
	PSPID    string `json:"psp_id"`
}

// Buyer is the buyer's identity as the minting app knew it.
type Buyer struct {
	VerifiedEmail string `json:"verified_email,omitempty"`
	Username      string `json:"username,omitempty"`
}

// Offer is what the session sells, as minted.
type Offer struct {
	MerchantDisplayName string                     `json:"merchant_display_name"`
	Plan                billing.HostedCheckoutPlan `json:"plan"`
	DueToday            int64                      `json:"due_today,string"`
	Options             []Option                   `json:"options"`
	Buyer               Buyer                      `json:"buyer"`
}

// Session is one stored session. ID is set only on the value Get returns.
type Session struct {
	ID              string
	CustomerID      uuid.UUID
	PriceID         uuid.UUID
	Offer           Offer
	SuccessURL      string
	Origin          string
	Attempt         int32
	EngineSessionID *uuid.UUID
	ExpiresAt       time.Time
}

// Expired reports whether the session's time to pay has passed.
func (s Session) Expired(now time.Time) bool { return !s.ExpiresAt.After(now) }

// Spent reports whether the session's last attempt failed.
func (s Session) Spent() bool { return s.Attempt >= MaxAttempts }

// Option returns the minted option with id.
func (s Session) Option(id string) (Option, bool) {
	for _, option := range s.Offer.Options {
		if option.ID == id {
			return option, true
		}
	}
	return Option{}, false
}

// AttemptKey is the engine idempotency key of the session's current attempt:
// every submission of one attempt sends the same key. It is derived from the
// id's hash, never the id, and spelled without digit runs so the card-number
// guard cannot mistake it for a PAN.
func (s Session) AttemptKey() string {
	sum := sha256.Sum256([]byte(s.ID))
	letters := make([]byte, 0, 2*len(sum))
	for _, b := range sum {
		letters = append(letters, 'a'+b>>4, 'a'+b&0x0f)
	}
	return "hosted:" + string(letters) + ":" + strconv.Itoa(int(s.Attempt))
}

// NewID mints a session id: ocs_ and 256 random bits.
func NewID() (string, error) {
	raw := make([]byte, idBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate checkout session id: %w", err)
	}
	return idPrefix + hex.EncodeToString(raw), nil
}

// ValidID reports whether id has a minted id's shape.
func ValidID(id string) bool {
	if len(id) != len(idPrefix)+2*idBytes || !strings.HasPrefix(id, idPrefix) {
		return false
	}
	for _, c := range id[len(idPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// IDHash is the stored form of id, also safe to log and to key limits on.
func IDHash(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return sum[:]
}

// Store reads and writes sessions for the request's merchant.
type Store struct{ db *db.DB }

func NewStore(database *db.DB) *Store { return &Store{db: database} }

// Create stores a new session under id.
func (s *Store) Create(ctx context.Context, id string, session Session, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	offer, err := json.Marshal(session.Offer)
	if err != nil {
		return fmt.Errorf("encode checkout offer: %w", err)
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), session.CustomerID); err != nil {
			return err
		}
		n, err := q.CreateHostedCheckoutSession(ctx, gen.CreateHostedCheckoutSessionParams{
			MerchantID: mid.UUID(), IDHash: IDHash(id), CustomerID: session.CustomerID, PriceID: session.PriceID,
			Offer: offer, SuccessUrl: session.SuccessURL, Origin: session.Origin,
			ExpiresAt: session.ExpiresAt, PurgeAt: session.ExpiresAt.Add(ReconciliationWindow), Now: now,
		})
		if err == nil && n == 0 {
			err = errors.New("checkout session id collision")
		}
		return err
	})
}

// Get returns the session for id, ErrNotFound when it does not exist or its
// reconciliation window has passed.
func (s *Store) Get(ctx context.Context, id string, now time.Time) (Session, error) {
	if !ValidID(id) {
		return Session{}, ErrNotFound
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Session{}, err
	}
	row, err := s.db.Gen(ctx).GetHostedCheckoutSession(ctx, gen.GetHostedCheckoutSessionParams{MerchantID: mid.UUID(), IDHash: IDHash(id), Now: now})
	if db.IsNotFound(err) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	out := Session{ID: id, CustomerID: row.CustomerID, PriceID: row.PriceID, SuccessURL: row.SuccessUrl, Origin: row.Origin,
		Attempt: row.Attempt, EngineSessionID: row.EngineSessionID, ExpiresAt: row.ExpiresAt}
	if err := json.Unmarshal(row.Offer, &out.Offer); err != nil {
		return Session{}, fmt.Errorf("decode checkout offer: %w", err)
	}
	return out, nil
}

// Bind records the engine session the current attempt created. An attempt
// already bound or advanced is left alone.
func (s *Store) Bind(ctx context.Context, session Session, engineSessionID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.Gen(ctx).BindHostedCheckoutEngineSession(ctx, gen.BindHostedCheckoutEngineSessionParams{
		MerchantID: mid.UUID(), IDHash: IDHash(session.ID), Attempt: session.Attempt, EngineSessionID: engineSessionID,
	})
	return err
}

// Advance moves past the session's terminally failed attempt and returns the
// session as it now stands; a concurrent caller may have advanced it first.
// Past its last attempt the session is Spent.
func (s *Store) Advance(ctx context.Context, session Session, now time.Time) (Session, error) {
	if session.Spent() {
		return session, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Session{}, err
	}
	if _, err := s.db.Gen(ctx).AdvanceHostedCheckoutAttempt(ctx, gen.AdvanceHostedCheckoutAttemptParams{
		MerchantID: mid.UUID(), IDHash: IDHash(session.ID), Attempt: session.Attempt, Now: now,
	}); err != nil {
		return Session{}, err
	}
	return s.Get(ctx, session.ID, now)
}

// DeleteExpired deletes up to limit sessions past their reconciliation
// window, across every merchant.
func DeleteExpired(ctx context.Context, database *db.DB, now time.Time, limit int32) (int64, error) {
	return database.GenDirectory().DeleteExpiredHostedCheckoutSessions(ctx, gen.DeleteExpiredHostedCheckoutSessionsParams{Now: now, RowLimit: limit})
}
