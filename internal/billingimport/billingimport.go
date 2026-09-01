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
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DeclaredCustomer ensures an openrails.customers row for a host subject.
type DeclaredCustomer struct {
	Customer uuid.UUID `json:"customer"` // the host's stable subject id (= openrails customers.id)
	Email    string    `json:"email,omitempty"`
}

// PSPRef names the PSP a declared row belongs to: either the openrails.psps
// row id, or the merchant's manifest PSP key (e.g. "mobius"). Exactly one form
// is set. or#893 removed the "unbound legacy lane" — an imported provider row
// carries the same attribution a pulled one does, because it is the same row
// and the same prune/rollback/uniqueness rules apply to it.
type PSPRef struct {
	ID  *uuid.UUID `json:"id,omitempty"`
	Key string     `json:"key,omitempty"`
}

// IsZero reports whether the ref names nothing.
func (r PSPRef) IsZero() bool {
	return (r.ID == nil || *r.ID == uuid.Nil) && strings.TrimSpace(r.Key) == ""
}

func (r PSPRef) String() string {
	if r.ID != nil && *r.ID != uuid.Nil {
		return r.ID.String()
	}
	return strings.TrimSpace(r.Key)
}

// DeclaredPaymentMethod is a stored instrument fact (e.g. an NMI vault entry).
// Idempotent by (rail, rail_customer_ref, rail_method_ref).
type DeclaredPaymentMethod struct {
	Customer uuid.UUID `json:"customer"`
	Rail     string    `json:"rail"`
	// PSP is the account that holds the vault entry. Falls back to
	// Options.DefaultPSP; missing both refuses the import.
	PSP                  PSPRef    `json:"psp,omitzero"`
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
	// ScheduleLive: the provider-side recurring schedule was NOT confirmed dead
	// at AsOf (e.g. a legacy NMI schedule that may keep rebilling after the
	// cancel). For rails with RemoteDeleteOnTerminalCancel the import stamps
	// DeletionScheduledAt and enqueues the deferred delete intent atomically.
	ScheduleLive bool `json:"schedule_live,omitempty"`
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
	// PSP binds the row to the merchant's PSP that owns this subscription at
	// the provider. Falls back to Options.DefaultPSP; missing both refuses the
	// import (or#893 — the nullable psp_id lane is gone).
	PSP           PSPRef            `json:"psp,omitzero"`
	UserEmail     string            `json:"user_email,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	PaidThrough   *time.Time        `json:"paid_through,omitempty"`
	Cancel        CancelEvidence    `json:"cancel,omitempty"`
	Dunning       *DunningEvidence  `json:"dunning,omitempty"`
	PaymentMethod *PaymentMethodRef `json:"payment_method,omitempty"`
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
	// (absence proof — every local subscription it omits is CANCELLED). MUST be
	// false for batched imports.
	SubscriptionsExhaustive bool `json:"subscriptions_exhaustive,omitempty"`
	// ExpectedSubscriptions is the typed confirmation required alongside
	// SubscriptionsExhaustive (or#858): how many subscriptions the exhaustive
	// book contains. A mismatch refuses the whole import — a partial batch
	// declared exhaustive cannot slip through as a boolean typo.
	ExpectedSubscriptions *int `json:"expected_subscriptions,omitempty"`
	// DefaultPSP attributes every declared row that names no PSP of its own
	// (or#893). A legacy book usually came from ONE gateway account, so stating
	// it once is the ordinary shape; per-row `psp` is for a mixed book. A row
	// that resolves to neither REFUSES the whole import — there is no
	// unattributed lane. It lives on the BODY, not on Options, so the HTTP door
	// (POST /v1/import/billing) reaches it the same way an embedded host does.
	DefaultPSP     PSPRef                  `json:"default_psp,omitzero"`
	Customers      []DeclaredCustomer      `json:"customers,omitempty"`
	PaymentMethods []DeclaredPaymentMethod `json:"payment_methods,omitempty"`
	Subscriptions  []DeclaredSubscription  `json:"subscriptions,omitempty"`
	Transactions   []DeclaredTransaction   `json:"transactions,omitempty"`
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
// (rail, transaction_id). Runs in a single merchant-scoped transaction (RLS):
// infrastructure failures roll back the whole declared book, while per-source
// business blocks remain ordinary committed outcomes for the other rows.
func Import(ctx context.Context, opts Options) (Result, error) {
	res := Result{Reasons: map[string]string{}}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Book.AsOf.IsZero() {
		return res, fmt.Errorf("import billing: Book.AsOf (evidence horizon) is required")
	}
	asOf := opts.Book.AsOf.UTC()

	database, err := openDB(ctx, opts.Config, opts.PGXPool)
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

	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		qx := txdb.Qx(ctx)
		q := txdb.Gen(ctx)

		// Lifecycle clock pinned at AsOf: every lifecycle write (ended_at, grace,
		// updated_at) is dated at the horizon — deterministic re-runs. The service
		// and deferred-delete scheduler share this transaction so subscription,
		// evidence, payment and intent writes cannot partially commit.
		lc := subscriptions.NewSubscriptionLifecycleService(txdb, nil, nil, nil, nil, nil, clockwork.NewFakeClockAt(asOf))
		deferDelete := intents.NewNMIDeleteScheduler(txdb, nil, intents.OriginUser, "billing-import terminal cancel, remote may be alive")
		lc.SetDeferredDeleteScheduler(deferDelete)

		// or#893: every provider-bound row the import writes carries a PSP.
		// Resolve the merchant's catalog ONCE, then attribute each declared row
		// from its own PSP ref, falling back to the whole-import default.
		psps, err := newPSPResolver(ctx, q, merchantID.UUID(), opts.Book.DefaultPSP)
		if err != nil {
			return err
		}

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

		// Payment methods: idempotent by the PSP-scoped instrument identity. An
		// existing instrument is reusable only by the same customer and rail; the
		// assertion lives in this transaction so hosts do not need a racy precheck.
		pmIDs := map[string]uuid.UUID{}
		pmKey := func(psp uuid.UUID, rail, custRef, methodRef string) string {
			return psp.String() + "\x1f" + rail + "\x1f" + custRef + "\x1f" + methodRef
		}
		for _, pm := range opts.Book.PaymentMethods {
			if pm.Rail == "" || pm.RailCustomerRef == "" || pm.Customer == uuid.Nil {
				return fmt.Errorf("declared payment method requires rail, rail_customer_ref and customer")
			}
			pmPSP, err := psps.resolve(pm.PSP, pm.Rail, fmt.Sprintf("payment method %s/%s", pm.Rail, pm.RailCustomerRef))
			if err != nil {
				return err
			}
			var id, owner uuid.UUID
			var existingRail string
			err = qx.QueryRow(ctx,
				`SELECT id, customer_id, rail FROM openrails.payment_methods
				 WHERE merchant_id = $1 AND psp_id = $2 AND rail_customer_ref = $3 AND rail_method_ref = $4`,
				merchantID.UUID(), pmPSP, pm.RailCustomerRef, pm.RailMethodRef).
				Scan(&id, &owner, &existingRail)
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
					PspID:                pmPSP,
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
			} else if owner != pm.Customer {
				return fmt.Errorf("payment method %s/%s already belongs to customer %s, not %s", pm.Rail, pm.RailCustomerRef, owner, pm.Customer)
			} else if !strings.EqualFold(existingRail, pm.Rail) {
				return fmt.Errorf("payment method %s/%s is stored on rail %q, not %q", pm.Rail, pm.RailCustomerRef, existingRail, pm.Rail)
			}
			pmIDs[pmKey(pmPSP, pm.Rail, pm.RailCustomerRef, pm.RailMethodRef)] = id
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
			subPSP, err := psps.resolve(s.PSP, s.Rail, fmt.Sprintf("subscription %s", s.SourceID))
			if err != nil {
				return err
			}
			f := reconcile.DeclaredSubscriptionFact{
				SourceID:           s.SourceID,
				Customer:           s.Customer,
				PriceID:            s.Price,
				Rail:               s.Rail,
				RailSubscriptionID: s.RailSubscriptionID,
				PspID:              subPSP,
				UserEmail:          nilIfEmpty(s.UserEmail),
				StartedAt:          s.StartedAt.UTC(),
				PaidThrough:        s.PaidThrough,
				CancelKind:         reconcile.DeclaredCancelKind(s.Cancel.Kind),
				CancelAt:           s.Cancel.At,
				CancelScheduleLive: s.Cancel.ScheduleLive,
				Evidence:           s.Evidence,
			}
			if s.Dunning != nil {
				f.DunningLive = s.Dunning.ScheduleLive
				f.DunningRetries = s.Dunning.Retries
				f.DunningLastRetryAt = s.Dunning.LastRetryAt
			}
			if s.PaymentMethod != nil {
				key := pmKey(subPSP, s.PaymentMethod.Rail, s.PaymentMethod.RailCustomerRef, s.PaymentMethod.RailMethodRef)
				id, ok := pmIDs[key]
				if !ok {
					// Ref to an instrument that already exists locally (e.g. a
					// prior import created it) — resolve from the DB once.
					var existing, owner uuid.UUID
					err := qx.QueryRow(ctx,
						`SELECT id, customer_id FROM openrails.payment_methods
						 WHERE merchant_id = $1 AND psp_id = $2 AND rail = $3 AND rail_customer_ref = $4 AND rail_method_ref = $5`,
						merchantID.UUID(), subPSP, s.PaymentMethod.Rail, s.PaymentMethod.RailCustomerRef, s.PaymentMethod.RailMethodRef).
						Scan(&existing, &owner)
					if err == nil {
						if owner != s.Customer {
							return fmt.Errorf("resolve payment method ref %s: instrument belongs to customer %s, not %s", s.SourceID, owner, s.Customer)
						}
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

		outcomes, err := reconcile.ImportDeclaredSubscriptions(ctx, txdb, lc, deferDelete, merchantID.UUID(), facts, txns, reconcile.DeclaredCoverage{
			SubscriptionsExhaustive: opts.Book.SubscriptionsExhaustive,
			ExpectedSubscriptions:   opts.Book.ExpectedSubscriptions,
		}, asOf)
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

// openDB wraps a caller pool (borrowed; Close is a no-op) or opens from config.
func openDB(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
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
	database, err := db.NewDB(ctx, cfg.DB)
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
