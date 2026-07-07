// Package billingimport is the #737 DeclaredBilling import seam: a host (or a
// SaaS merchant over HTTP) hands over its legacy billing book as FACTS and
// OpenRails classifies them through the same decider pipeline the
// pull/probe/webhook planes use, evaluated at the declared AsOf horizon.
// The #636 ImportAdminGrants pattern generalized: SourceID idempotency,
// per-source result lists, merchant-scoped RLS connection.
//
// It lives under internal/ (not pkg/embedded) so the HTTP surface
// (internal/http/handlers) can share it without an import cycle;
// pkg/embedded aliases these types as its public vocabulary. The JSON tags
// are the POST /v1/import/billing wire shape: times RFC3339, uuids as
// strings, amounts provider-wire CENTS (amount_cents).
package billingimport

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DeclaredCustomer ensures an openrails.customers row for a host subject.
type DeclaredCustomer struct {
	Customer uuid.UUID `json:"customer"` // the host's stable subject id (= openrails customers.id)
	Email    string    `json:"email,omitempty"`
}

// DeclaredPaymentMethod is a stored instrument fact (e.g. an NMI vault entry).
// Idempotent by (rail, rail_customer_ref, rail_method_ref).
type DeclaredPaymentMethod struct {
	Customer             uuid.UUID `json:"customer"`
	Rail                 string    `json:"rail"`
	RailCustomerRef      string    `json:"rail_customer_ref"` // e.g. NMI customer_vault_id
	RailMethodRef        string    `json:"rail_method_ref"`   // e.g. NMI billing_id; "" for one-instrument vaults
	InitialTransactionID string    `json:"initial_transaction_id,omitempty"`
	LastFour             string    `json:"last_four,omitempty"`
	CardType             string    `json:"card_type,omitempty"`
	ExpiryDate           string    `json:"expiry_date,omitempty"`
	CreatedAt            time.Time `json:"created_at,omitempty"`
}

// PaymentMethodRef links a DeclaredSubscription to a DeclaredPaymentMethod
// (dunning rebills need the subscription→vault linkage).
type PaymentMethodRef struct {
	Rail            string `json:"rail"`
	RailCustomerRef string `json:"rail_customer_ref"`
	RailMethodRef   string `json:"rail_method_ref"`
}

// CancelEvidence is explicit, settled cancel history. Kinds: "" (none),
// "user_cancelled", "chargeback", "provider_terminated". A declared cancel is
// written faithfully (cancel_type + dates); only evidence-less subscriptions
// go through the decider.
type CancelEvidence struct {
	Kind string    `json:"kind,omitempty"`
	At   time.Time `json:"at,omitempty"`
}

// DunningEvidence is the host's legacy dunning state at AsOf.
type DunningEvidence struct {
	Retries      int        `json:"retries"`
	LastRetryAt  *time.Time `json:"last_retry_at,omitempty"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	ScheduleLive bool       `json:"schedule_live"` // the legacy rebill schedule was still retrying at AsOf
}

// DeclaredTransaction is one charge-level fact (success AND declines — the true
// attempt history). AmountCents is provider-wire CENTS; conversion to ledger
// micros happens inside OpenRails at the one existing boundary.
type DeclaredTransaction struct {
	RailSubscriptionID string    `json:"rail_subscription_id"`
	TransactionID      string    `json:"transaction_id"`
	Type               string    `json:"type,omitempty"` // sale | refund | chargeback | decline; "" = sale
	Success            bool      `json:"success"`
	AmountCents        int64     `json:"amount_cents"`
	Currency           string    `json:"currency"`
	OccurredAt         time.Time `json:"occurred_at"`
}

// DeclaredSubscription is one subscription's facts, not classifications.
type DeclaredSubscription struct {
	SourceID           string    `json:"source_id"` // host's stable id — idempotency of reporting + audit
	Customer           uuid.UUID `json:"customer"`
	Price              uuid.UUID `json:"price"`
	Rail               string    `json:"rail"`
	RailSubscriptionID string    `json:"rail_subscription_id"` // required; synthesize a stable id for rail-less legacy rows
	// RailMerchantAccountID binds the row to its operator-declared
	// openrails.rail_merchant_accounts row (nil = unbound legacy lane).
	RailMerchantAccountID *uuid.UUID        `json:"rail_merchant_account_id,omitempty"`
	UserEmail             string            `json:"user_email,omitempty"`
	StartedAt             time.Time         `json:"started_at"`
	PaidThrough           *time.Time        `json:"paid_through,omitempty"`
	Cancel                CancelEvidence    `json:"cancel,omitempty"`
	Dunning               *DunningEvidence  `json:"dunning,omitempty"`
	PaymentMethod         *PaymentMethodRef `json:"payment_method,omitempty"`
	// Evidence is the host's verbatim legacy payload, stored on
	// subscriptions.gateway_response at seed (forensics; never re-parsed).
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// DeclaredBilling is the import body: one merchant's book (or one batch of it).
type DeclaredBilling struct {
	// AsOf is the evidence horizon — when the host's data was true. ALL
	// classification evaluates against it, never wall-clock, so the same book
	// classifies identically whenever the import runs.
	AsOf time.Time `json:"as_of"`
	// SubscriptionsExhaustive: this call covers the merchant's ENTIRE book
	// (absence proof). MUST be false for batched imports.
	SubscriptionsExhaustive bool                    `json:"subscriptions_exhaustive,omitempty"`
	Customers               []DeclaredCustomer      `json:"customers,omitempty"`
	PaymentMethods          []DeclaredPaymentMethod `json:"payment_methods,omitempty"`
	Subscriptions           []DeclaredSubscription  `json:"subscriptions,omitempty"`
	Transactions            []DeclaredTransaction   `json:"transactions,omitempty"`
}

// Options configures Import. Merchant scoping: MerchantID when the caller
// already resolved it (the HTTP door — credential-derived), else MerchantSlug.
type Options struct {
	Config       *config.Config
	PGXPool      *pgxpool.Pool
	MerchantSlug string
	MerchantID   merchant.ID
	Book         DeclaredBilling
}

// Result reports per-SourceID outcomes (subscriptions only; customers/payment
// methods are idempotent upserts with no per-row audit).
type Result struct {
	Imported []string          `json:"imported"`
	Skipped  []string          `json:"skipped"` // already present (idempotent re-run)
	Blocked  []string          `json:"blocked"`
	Reasons  map[string]string `json:"reasons"` // SourceID → block reason
}

// Import lands a host-declared billing book. Explicitly-cancelled facts
// are written directly (settled history, faithful cancel_type/dates); the
// ambiguous cohort is seeded `unknown` and resolved by the #665 decider against
// the declared snapshot at AsOf — park-as-unknown and cancellation-last-resort
// hold server-side by construction. Charges land idempotently by
// (rail, transaction_id). Runs in a single merchant-scoped connection (RLS).
func Import(ctx context.Context, opts Options) (Result, error) {
	res := Result{Reasons: map[string]string{}}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Book.AsOf.IsZero() {
		return res, fmt.Errorf("import billing: Book.AsOf (evidence horizon) is required")
	}
	asOf := opts.Book.AsOf.UTC()

	database, err := openDB(opts.Config, opts.PGXPool)
	if err != nil {
		return res, err
	}
	defer database.Close()

	merchantID := opts.MerchantID
	if merchantID.IsZero() {
		merchantID, err = db.ResolveMerchantSlug(ctx, database.Pool(), strings.TrimSpace(opts.MerchantSlug))
		if err != nil {
			return res, fmt.Errorf("import billing: resolve merchant %q: %w", opts.MerchantSlug, err)
		}
	}
	ctx = merchant.WithID(ctx, merchantID)

	// Lifecycle clock pinned at AsOf: every lifecycle write (ended_at, grace,
	// updated_at) is dated at the horizon — deterministic re-runs.
	lc := subscriptions.NewSubscriptionLifecycleService(database, nil, nil, nil, nil, nil, clockwork.NewFakeClockAt(asOf))
	// The import seam has no River producer to enqueue an nmi_delete intent
	// inline, but a terminal cancel through the decider (ResolveCancelledRemoteAlive)
	// must still leave the DeletionScheduledAt marker durably — see
	// markerOnlyDeferredDelete below.
	lc.SetDeferredDeleteScheduler(markerOnlyDeferredDelete{})

	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		qx := database.Qx(ctx)
		q := database.Gen(ctx)

		// Customers first (subscriptions FK them).
		seen := map[uuid.UUID]struct{}{}
		for _, c := range opts.Book.Customers {
			if c.Customer == uuid.Nil {
				continue
			}
			if err := db.EnsureCustomerRow(ctx, qx, merchantID.UUID(), c.Customer); err != nil {
				return fmt.Errorf("ensure customer %s: %w", c.Customer, err)
			}
			seen[c.Customer] = struct{}{}
		}
		for _, s := range opts.Book.Subscriptions {
			if s.Customer == uuid.Nil {
				continue
			}
			if _, ok := seen[s.Customer]; ok {
				continue
			}
			if err := db.EnsureCustomerRow(ctx, qx, merchantID.UUID(), s.Customer); err != nil {
				return fmt.Errorf("ensure customer %s: %w", s.Customer, err)
			}
			seen[s.Customer] = struct{}{}
		}

		// Payment methods: idempotent by (rail, rail_customer_ref, rail_method_ref).
		pmIDs := map[string]uuid.UUID{}
		pmKey := func(rail, custRef, methodRef string) string { return rail + "\x1f" + custRef + "\x1f" + methodRef }
		for _, pm := range opts.Book.PaymentMethods {
			if pm.Rail == "" || pm.RailCustomerRef == "" || pm.Customer == uuid.Nil {
				return fmt.Errorf("declared payment method requires rail, rail_customer_ref and customer")
			}
			var id uuid.UUID
			err := qx.QueryRow(ctx,
				`SELECT id FROM openrails.payment_methods
				 WHERE merchant_id = $1 AND rail = $2 AND rail_customer_ref = $3 AND rail_method_ref = $4`,
				merchantID.UUID(), pm.Rail, pm.RailCustomerRef, pm.RailMethodRef).Scan(&id)
			if err == pgx.ErrNoRows {
				id = uuid.New()
				created := pm.CreatedAt.UTC()
				if created.IsZero() {
					created = asOf
				}
				if _, err := q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
					ID:                   id,
					MerchantID:           merchantID.UUID(),
					CustomerID:           pm.Customer,
					Rail:                 pm.Rail,
					RailCustomerRef:      pm.RailCustomerRef,
					RailMethodRef:        pm.RailMethodRef,
					InitialTransactionID: pm.InitialTransactionID,
					LastFour:             nilIfEmpty(pm.LastFour),
					CardType:             nilIfEmpty(pm.CardType),
					ExpiryDate:           nilIfEmpty(pm.ExpiryDate),
					CreatedAt:            created,
					UpdatedAt:            created,
					RebillDriver:         "",
				}); err != nil {
					return fmt.Errorf("create payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
				}
			} else if err != nil {
				return fmt.Errorf("lookup payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
			}
			pmIDs[pmKey(pm.Rail, pm.RailCustomerRef, pm.RailMethodRef)] = id
		}

		// Transactions → the declared snapshot's charge events.
		txns := make([]reconcile.RemoteTransaction, 0, len(opts.Book.Transactions))
		for _, t := range opts.Book.Transactions {
			typ := reconcile.TransactionType(t.Type)
			if t.Type == "" {
				typ = reconcile.TransactionTypeSale
			}
			txns = append(txns, reconcile.RemoteTransaction{
				TransactionID:  t.TransactionID,
				SubscriptionID: t.RailSubscriptionID,
				Type:           typ,
				Success:        t.Success,
				AmountCents:    t.AmountCents,
				Currency:       t.Currency,
				OccurredAt:     t.OccurredAt.UTC(),
			})
		}

		facts := make([]reconcile.DeclaredSubscriptionFact, 0, len(opts.Book.Subscriptions))
		for _, s := range opts.Book.Subscriptions {
			f := reconcile.DeclaredSubscriptionFact{
				SourceID:              s.SourceID,
				Customer:              s.Customer,
				PriceID:               s.Price,
				Rail:                  s.Rail,
				RailSubscriptionID:    s.RailSubscriptionID,
				RailMerchantAccountID: s.RailMerchantAccountID,
				UserEmail:             nilIfEmpty(s.UserEmail),
				StartedAt:             s.StartedAt.UTC(),
				PaidThrough:           s.PaidThrough,
				CancelKind:            reconcile.DeclaredCancelKind(s.Cancel.Kind),
				CancelAt:              s.Cancel.At,
				Evidence:              s.Evidence,
			}
			if s.Dunning != nil {
				f.DunningLive = s.Dunning.ScheduleLive
				f.DunningRetries = s.Dunning.Retries
				f.DunningLastRetryAt = s.Dunning.LastRetryAt
			}
			if s.PaymentMethod != nil {
				key := pmKey(s.PaymentMethod.Rail, s.PaymentMethod.RailCustomerRef, s.PaymentMethod.RailMethodRef)
				id, ok := pmIDs[key]
				if !ok {
					// Ref to an instrument that already exists locally (e.g. a
					// prior import created it) — resolve from the DB once.
					var existing uuid.UUID
					err := qx.QueryRow(ctx,
						`SELECT id FROM openrails.payment_methods
						 WHERE merchant_id = $1 AND rail = $2 AND rail_customer_ref = $3 AND rail_method_ref = $4`,
						merchantID.UUID(), s.PaymentMethod.Rail, s.PaymentMethod.RailCustomerRef, s.PaymentMethod.RailMethodRef).Scan(&existing)
					if err == nil {
						id, ok = existing, true
						pmIDs[key] = existing
					} else if err != pgx.ErrNoRows {
						return fmt.Errorf("resolve payment method ref %s: %w", s.SourceID, err)
					}
				}
				if ok {
					f.PaymentMethodID = &id
				}
			}
			facts = append(facts, f)
		}

		outcomes, err := reconcile.ImportDeclaredSubscriptions(ctx, database, lc, merchantID.UUID(), facts, txns, opts.Book.SubscriptionsExhaustive, asOf)
		if err != nil {
			return err
		}
		for src, o := range outcomes {
			switch o.Code {
			case reconcile.DeclaredImported:
				res.Imported = append(res.Imported, src)
			case reconcile.DeclaredAlreadyPresent:
				res.Skipped = append(res.Skipped, src)
			default:
				res.Blocked = append(res.Blocked, src)
				res.Reasons[src] = o.Reason
			}
		}
		sort.Strings(res.Imported)
		sort.Strings(res.Skipped)
		sort.Strings(res.Blocked)
		return nil
	})
	return res, err
}

// markerOnlyDeferredDelete implements subscriptions.DeferredDeleteScheduler
// by doing nothing beyond letting the caller stamp DeletionScheduledAt: it
// exists only so the `s.deferDelete != nil` gate in ResolveUnknownSubscription
// passes, so a terminal import-time cancel where the remote NMI schedule may
// still be retrying leaves the marker on disk instead of losing it to a WARN
// log. There is no River producer in the import path to enqueue an
// nmi_delete intent inline — the marker alone is enough: the host's boot
// sweep (app.ConvertDeferredDeleteMarkersToIntents) already exists precisely
// to pick up out-of-band markers (direct-DB imports, #391) and convert them
// to durable intents on the next boot. Doctrine: the marker means "remote
// schedule may still be live; dispose when armed", never "already deleted".
type markerOnlyDeferredDelete struct{}

func (markerOnlyDeferredDelete) ScheduleNMIDelete(context.Context, string, uuid.UUID, time.Time) error {
	return nil
}

func (markerOnlyDeferredDelete) CancelNMIDelete(context.Context, string, uuid.UUID) error {
	return nil
}

func (m markerOnlyDeferredDelete) WithTx(pgx.Tx) subscriptions.DeferredDeleteScheduler {
	return m
}

// openDB wraps a caller pool (borrowed; Close is a no-op) or opens from config.
func openDB(cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if pool != nil {
		schema := config.DefaultSchema
		if cfg != nil && cfg.DB != nil {
			schema = cfg.DB.SchemaName()
		}
		return db.NewWithPGXPool(pool, schema)
	}
	if cfg == nil || cfg.DB == nil {
		return nil, fmt.Errorf("config database is required")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return database, nil
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}
