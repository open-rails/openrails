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

// DeclaredPaymentMethod is a stored instrument fact (e.g. an NMI vault entry).
// Idempotent by (rail, rail_customer_ref, rail_method_ref).
type DeclaredPaymentMethod struct {
	SourceID             string          `json:"source_id"`
	ID                   uuid.UUID       `json:"id,omitempty"`
	PSPKey               string          `json:"psp_key,omitempty"`
	Customer             uuid.UUID       `json:"customer"`
	Rail                 string          `json:"rail"`
	RailCustomerRef      string          `json:"rail_customer_ref"` // e.g. NMI customer_vault_id
	RailMethodRef        string          `json:"rail_method_ref"`   // e.g. NMI billing_id; "" for one-instrument vaults
	InitialTransactionID string          `json:"initial_transaction_id,omitempty"`
	LastFour             string          `json:"last_four,omitempty"`
	CardType             string          `json:"card_type,omitempty"`
	ExpiryDate           string          `json:"expiry_date,omitempty"`
	CreatedAt            time.Time       `json:"created_at,omitempty"`
	UpdatedAt            time.Time       `json:"updated_at,omitempty"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
}

// PaymentMethodRef links a DeclaredSubscription to a DeclaredPaymentMethod
// (dunning rebills need the subscription→vault linkage).
type PaymentMethodRef struct {
	Rail            string `json:"rail"`
	PSPKey          string `json:"psp_key,omitempty"`
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
	// PSPKey resolves the merchant manifest key (for example mobius) inside
	// OpenRails.
	PSPKey        string            `json:"psp_key,omitempty"`
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

// DeclaredPayment is one successful historical charge that is not represented
// by the subscription transaction facts above.
type DeclaredPayment struct {
	SourceID      string          `json:"source_id"`
	Customer      uuid.UUID       `json:"customer"`
	Price         uuid.UUID       `json:"price"`
	Subscription  *uuid.UUID      `json:"subscription,omitempty"`
	Rail          string          `json:"rail"`
	PSPKey        string          `json:"psp_key,omitempty"`
	TransactionID string          `json:"transaction_id"`
	AmountMicros  int64           `json:"amount_micros"`
	Currency      string          `json:"currency"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// DeclaredDunningEvent is display/forensics evidence from a legacy billing
// system. It never drives money or entitlement decisions.
type DeclaredDunningEvent struct {
	SourceID     string          `json:"source_id"`
	ID           uuid.UUID       `json:"id"`
	Subscription *uuid.UUID      `json:"subscription,omitempty"`
	Customer     *uuid.UUID      `json:"customer,omitempty"`
	EventType    string          `json:"event_type"`
	Rail         string          `json:"rail"`
	OccurredAt   time.Time       `json:"occurred_at"`
	Source       string          `json:"source"`
	Detail       json.RawMessage `json:"detail,omitempty"`
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
	Payments                []DeclaredPayment       `json:"payments,omitempty"`
	DunningEvents           []DeclaredDunningEvent  `json:"dunning_events,omitempty"`
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

// Result reports per-SourceID outcomes for each declared writer lane.
type Result struct {
	Imported []string          `json:"imported"`
	Skipped  []string          `json:"skipped"` // already present (idempotent re-run)
	Blocked  []string          `json:"blocked"`
	Reasons  map[string]string `json:"reasons"` // SourceID → block reason

	PaymentsImported []string `json:"payments_imported,omitempty"`
	PaymentsSkipped  []string `json:"payments_skipped,omitempty"`
	DunningImported  []string `json:"dunning_imported,omitempty"`
	DunningSkipped   []string `json:"dunning_skipped,omitempty"`

	PaymentMethodsImported []string          `json:"payment_methods_imported,omitempty"`
	PaymentMethodsSkipped  []string          `json:"payment_methods_skipped,omitempty"`
	PaymentMethodsBlocked  []string          `json:"payment_methods_blocked,omitempty"`
	PaymentMethodReasons   map[string]string `json:"payment_method_reasons,omitempty"`
}

// Import lands a host-declared billing book. Explicitly-cancelled facts
// are written directly (settled history, faithful cancel_type/dates); the
// ambiguous cohort is seeded `unknown` and resolved by the #665 decider against
// the declared snapshot at AsOf — park-as-unknown and cancellation-last-resort
// hold server-side by construction. Charges land idempotently by
// (rail, transaction_id). Runs in a single merchant-scoped connection (RLS).
func Import(ctx context.Context, opts Options) (Result, error) {
	res := Result{
		Reasons:              map[string]string{},
		PaymentMethodReasons: map[string]string{},
	}
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
	// A terminal cancel — through the decider (ResolveCancelledRemoteAlive) or
	// declared explicitly with a live remote schedule — must durably record the
	// owed remote NMI delete. Enqueue the real ledger intent inline, same as the
	// runtime producers — user-origin (these are the source system's user
	// cancellations; system-origin would park under mode=limited), no rate
	// ceiling (batch declaration of settled facts, not self-service).
	deferDelete := intents.NewNMIDeleteScheduler(database, nil, intents.OriginUser, "billing-import terminal cancel, remote may be alive")
	lc.SetDeferredDeleteScheduler(deferDelete)

	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		qx := database.Qx(ctx)
		q := database.Gen(ctx)

		pspIDs := map[string]*uuid.UUID{}
		resolvePSPID := func(rail, key string) (*uuid.UUID, error) {
			key = strings.TrimSpace(key)
			if key == "" {
				return nil, nil
			}
			rail = strings.ToLower(strings.TrimSpace(rail))
			cacheKey := rail + "\x1f" + key
			if id, ok := pspIDs[cacheKey]; ok {
				return id, nil
			}
			rows, err := q.ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
				MerchantID: merchantID.UUID(),
				Rail:       &rail,
			})
			if err != nil {
				return nil, fmt.Errorf("list psps for rail %q: %w", rail, err)
			}
			var found *uuid.UUID
			for _, row := range rows {
				if row.Archived || row.Environment != "live" || row.Key == nil ||
					strings.TrimSpace(*row.Key) != key {
					continue
				}
				if found != nil {
					return nil, fmt.Errorf("multiple active live psps %q for rail %q", key, rail)
				}
				id := row.ID
				found = &id
			}
			if found != nil {
				pspIDs[cacheKey] = found
				return found, nil
			}
			return nil, fmt.Errorf("active live psp %q for rail %q not found", key, rail)
		}

		// Customers first (subscriptions FK them).
		seen := map[uuid.UUID]struct{}{}
		ensureCustomer := func(customer uuid.UUID) error {
			if customer == uuid.Nil {
				return nil
			}
			if _, ok := seen[customer]; ok {
				return nil
			}
			if err := db.EnsureCustomerRow(ctx, qx, merchantID.UUID(), customer); err != nil {
				return fmt.Errorf("ensure customer %s: %w", customer, err)
			}
			seen[customer] = struct{}{}
			return nil
		}
		for _, c := range opts.Book.Customers {
			if err := ensureCustomer(c.Customer); err != nil {
				return err
			}
		}
		for _, s := range opts.Book.Subscriptions {
			if err := ensureCustomer(s.Customer); err != nil {
				return err
			}
		}
		for _, pm := range opts.Book.PaymentMethods {
			if err := ensureCustomer(pm.Customer); err != nil {
				return err
			}
		}
		for _, payment := range opts.Book.Payments {
			if err := ensureCustomer(payment.Customer); err != nil {
				return err
			}
		}
		for _, event := range opts.Book.DunningEvents {
			if event.Customer != nil {
				if err := ensureCustomer(*event.Customer); err != nil {
					return err
				}
			}
		}

		// Payment methods: idempotent by (rail, rail_customer_ref, rail_method_ref).
		pmIDs := map[string]uuid.UUID{}
		pmKey := func(rail string, pspID *uuid.UUID, custRef, methodRef string) string {
			psp := ""
			if pspID != nil {
				psp = pspID.String()
			}
			return rail + "\x1f" + psp + "\x1f" + custRef + "\x1f" + methodRef
		}
		for _, pm := range opts.Book.PaymentMethods {
			if strings.TrimSpace(pm.SourceID) == "" || pm.Rail == "" ||
				pm.RailCustomerRef == "" || pm.Customer == uuid.Nil {
				return fmt.Errorf("declared payment method requires source_id, rail, rail_customer_ref and customer")
			}
			pspID, err := resolvePSPID(pm.Rail, pm.PSPKey)
			if err != nil {
				return fmt.Errorf("resolve payment method psp %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
			}
			var (
				id         uuid.UUID
				customerID uuid.UUID
			)
			err = qx.QueryRow(ctx,
				`SELECT id, customer_id FROM openrails.payment_methods
				 WHERE merchant_id = $1 AND rail = $2 AND rail_customer_ref = $3
				   AND rail_method_ref = $4 AND psp_id IS NOT DISTINCT FROM $5::uuid`,
				merchantID.UUID(), pm.Rail, pm.RailCustomerRef, pm.RailMethodRef, pspID).
				Scan(&id, &customerID)
			if err == pgx.ErrNoRows {
				id = pm.ID
				if id == uuid.Nil {
					id = uuid.New()
				}
				created := pm.CreatedAt.UTC()
				if created.IsZero() {
					created = asOf
				}
				updated := pm.UpdatedAt.UTC()
				if updated.IsZero() {
					updated = created
				}
				if _, err := q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
					ID:                   id,
					MerchantID:           merchantID.UUID(),
					PspID:                pspID,
					CustomerID:           pm.Customer,
					Rail:                 pm.Rail,
					RailCustomerRef:      pm.RailCustomerRef,
					RailMethodRef:        pm.RailMethodRef,
					InitialTransactionID: pm.InitialTransactionID,
					LastFour:             nilIfEmpty(pm.LastFour),
					CardType:             nilIfEmpty(pm.CardType),
					ExpiryDate:           nilIfEmpty(pm.ExpiryDate),
					Metadata:             pm.Metadata,
					CreatedAt:            created,
					UpdatedAt:            updated,
					RebillDriver:         "",
				}); err != nil {
					return fmt.Errorf("create payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
				}
				res.PaymentMethodsImported = append(res.PaymentMethodsImported, pm.SourceID)
			} else if err != nil {
				return fmt.Errorf("lookup payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
			} else if customerID != pm.Customer {
				res.PaymentMethodsBlocked = append(res.PaymentMethodsBlocked, pm.SourceID)
				res.PaymentMethodReasons[pm.SourceID] = "payment method belongs to a different customer"
				continue
			} else {
				res.PaymentMethodsSkipped = append(res.PaymentMethodsSkipped, pm.SourceID)
			}
			pmIDs[pmKey(pm.Rail, pspID, pm.RailCustomerRef, pm.RailMethodRef)] = id
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
			pspID, err := resolvePSPID(s.Rail, s.PSPKey)
			if err != nil {
				return fmt.Errorf("resolve subscription psp %s: %w", s.SourceID, err)
			}
			f := reconcile.DeclaredSubscriptionFact{
				SourceID:           s.SourceID,
				Customer:           s.Customer,
				PriceID:            s.Price,
				Rail:               s.Rail,
				RailSubscriptionID: s.RailSubscriptionID,
				PspID:              pspID,
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
				refPSPID, err := resolvePSPID(s.PaymentMethod.Rail, s.PaymentMethod.PSPKey)
				if err != nil {
					return fmt.Errorf("resolve payment method ref psp %s: %w", s.SourceID, err)
				}
				key := pmKey(s.PaymentMethod.Rail, refPSPID, s.PaymentMethod.RailCustomerRef, s.PaymentMethod.RailMethodRef)
				id, ok := pmIDs[key]
				if !ok {
					// Ref to an instrument that already exists locally (e.g. a
					// prior import created it) — resolve from the DB once.
					var existing uuid.UUID
					err := qx.QueryRow(ctx,
						`SELECT id FROM openrails.payment_methods
						 WHERE merchant_id = $1 AND rail = $2 AND rail_customer_ref = $3
						   AND rail_method_ref = $4 AND psp_id IS NOT DISTINCT FROM $5::uuid`,
						merchantID.UUID(), s.PaymentMethod.Rail, s.PaymentMethod.RailCustomerRef,
						s.PaymentMethod.RailMethodRef, refPSPID).Scan(&existing)
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

		outcomes, err := reconcile.ImportDeclaredSubscriptions(ctx, database, lc, deferDelete, merchantID.UUID(), facts, txns, opts.Book.SubscriptionsExhaustive, asOf)
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

		writer := &reconcile.PGLocalWriter{DB: database, Now: func() time.Time { return asOf }}
		for _, payment := range opts.Book.Payments {
			if strings.TrimSpace(payment.SourceID) == "" || payment.Customer == uuid.Nil ||
				payment.Price == uuid.Nil || strings.TrimSpace(payment.Rail) == "" ||
				strings.TrimSpace(payment.TransactionID) == "" || payment.AmountMicros <= 0 ||
				strings.TrimSpace(payment.Currency) == "" || payment.OccurredAt.IsZero() {
				return fmt.Errorf("declared payment requires source_id, customer, price, rail, transaction_id, positive amount_micros, currency and occurred_at")
			}
			pspID, err := resolvePSPID(payment.Rail, payment.PSPKey)
			if err != nil {
				return fmt.Errorf("resolve payment psp %s: %w", payment.SourceID, err)
			}
			metadata, err := metadataMap(payment.Metadata)
			if err != nil {
				return fmt.Errorf("decode payment metadata %s: %w", payment.SourceID, err)
			}
			changed, err := writer.BackfillPayment(ctx, reconcile.BackfillPaymentAction{
				PspID:          pspID,
				Rail:           payment.Rail,
				TransactionID:  payment.TransactionID,
				AmountMicros:   &payment.AmountMicros,
				Currency:       payment.Currency,
				PurchasedAt:    payment.OccurredAt.UTC(),
				PriceID:        payment.Price,
				SubscriptionID: payment.Subscription,
				CustomerID:     payment.Customer,
				Metadata:       metadata,
			})
			if err != nil {
				return fmt.Errorf("import payment %s: %w", payment.SourceID, err)
			}
			if !changed {
				var customerID uuid.UUID
				err := qx.QueryRow(ctx,
					`SELECT customer_id FROM openrails.payments
					 WHERE merchant_id = $1 AND rail = $2 AND transaction_id = $3
					   AND psp_id IS NOT DISTINCT FROM $4::uuid`,
					merchantID.UUID(), payment.Rail, payment.TransactionID, pspID).
					Scan(&customerID)
				if err != nil {
					return fmt.Errorf("resolve existing payment %s: %w", payment.SourceID, err)
				}
				if customerID != payment.Customer {
					return fmt.Errorf("payment %s belongs to a different customer", payment.SourceID)
				}
			}
			if changed {
				res.PaymentsImported = append(res.PaymentsImported, payment.SourceID)
			} else {
				res.PaymentsSkipped = append(res.PaymentsSkipped, payment.SourceID)
			}
		}
		for _, event := range opts.Book.DunningEvents {
			if strings.TrimSpace(event.SourceID) == "" || event.ID == uuid.Nil ||
				strings.TrimSpace(event.EventType) == "" || strings.TrimSpace(event.Rail) == "" ||
				event.OccurredAt.IsZero() || strings.TrimSpace(event.Source) == "" {
				return fmt.Errorf("declared dunning event requires source_id, id, event_type, rail, occurred_at and source")
			}
			changed, err := q.InsertImportedDunningHistory(ctx, gen.InsertImportedDunningHistoryParams{
				ID:             event.ID,
				MerchantID:     merchantID.UUID(),
				SubscriptionID: event.Subscription,
				CustomerID:     event.Customer,
				EventType:      event.EventType,
				Rail:           event.Rail,
				OccurredAt:     event.OccurredAt.UTC(),
				Source:         event.Source,
				Detail:         event.Detail,
			})
			if err != nil {
				return fmt.Errorf("import dunning event %s: %w", event.SourceID, err)
			}
			if changed > 0 {
				res.DunningImported = append(res.DunningImported, event.SourceID)
			} else {
				res.DunningSkipped = append(res.DunningSkipped, event.SourceID)
			}
		}
		sort.Strings(res.PaymentsImported)
		sort.Strings(res.PaymentsSkipped)
		sort.Strings(res.DunningImported)
		sort.Strings(res.DunningSkipped)
		sort.Strings(res.PaymentMethodsImported)
		sort.Strings(res.PaymentMethodsSkipped)
		sort.Strings(res.PaymentMethodsBlocked)
		return nil
	})
	return res, err
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

func metadataMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	if metadata == nil {
		return nil, fmt.Errorf("metadata must be a json object")
	}
	return metadata, nil
}
