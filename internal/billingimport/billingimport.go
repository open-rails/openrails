// Package billingimport is the #737 DeclaredBilling import seam: a host (or a
// SaaS merchant over HTTP) hands over its legacy billing book as FACTS and
// OpenRails classifies them through the same decider pipeline the
// pull/probe/webhook planes use, evaluated at the declared AsOf horizon.
// Admin comps ride the same book (AdminGrants): SourceID idempotency,
// per-source result lists, one merchant-scoped transaction.
//
// The wire vocabulary (POST /v1/import/billing and Client.ImportBilling) is
// defined on the root openrails package; this package aliases it.
package billingimport

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The declared-facts vocabulary is the shared Client wire (openrails.DeclaredBilling);
// these aliases keep the engine-side names.
type (
	DeclaredCustomer      = openrails.DeclaredCustomer
	PSPRef                = openrails.PSPRef
	DeclaredPaymentMethod = openrails.DeclaredPaymentMethod
	PaymentMethodRef      = openrails.PaymentMethodRef
	CancelEvidence        = openrails.CancelEvidence
	DunningEvidence       = openrails.DunningEvidence
	DeclaredTransaction   = openrails.DeclaredTransaction
	DeclaredSubscription  = openrails.DeclaredSubscription
	DeclaredAdminGrant    = openrails.DeclaredAdminGrant
	DeclaredBilling       = openrails.DeclaredBilling
	Result                = openrails.BillingImportResult
)

// Options configures Import. MerchantID is the already resolved or authorized
// immutable merchant UUID; imports never interpret a public name.
type Options struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID merchant.ID
	Book       DeclaredBilling
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
	if err := database.RequireMerchantID(ctx, merchantID); err != nil {
		return res, err
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
		if err := importAdminGrants(ctx, q, merchantID.UUID(), opts.Book.AdminGrants, &res); err != nil {
			return err
		}
		sort.Strings(res.Imported)
		sort.Strings(res.Skipped)
		sort.Strings(res.Blocked)
		return nil
	})
	return res, err
}

// openDB wraps a caller pool (borrowed; Close is a no-op) or opens from
// config, and enforces the RLS posture either way: an import runs
// merchant-scoped writes, and a privileged role skips the policies that make
// that scoping real.
func openDB(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	var (
		database *db.DB
		err      error
	)
	if pool != nil {
		schema := config.DefaultSchema
		if cfg != nil && cfg.DB != nil {
			schema = cfg.DB.SchemaName()
		}
		database, err = db.NewWithPGXPool(pool, schema)
		if err != nil {
			return nil, err
		}
	} else {
		if cfg == nil || cfg.DB == nil {
			return nil, fmt.Errorf("config database is required")
		}
		database, err = db.NewDB(ctx, cfg.DB)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
	}
	if err := database.EnforceRLSPosture(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// importAdminGrants records each comp as a source_type=admin grant and
// materializes its window, idempotent by SourceID. A product without an
// entitlements_spec has nothing to grant and is reported as blocked.
func importAdminGrants(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, declared []DeclaredAdminGrant, res *Result) error {
	if len(declared) == 0 {
		return nil
	}
	ledger := grants.New(q, merchantID)
	specs := map[uuid.UUID][]string{}
	for _, g := range declared {
		if g.Customer == uuid.Nil || g.Product == uuid.Nil || strings.TrimSpace(g.SourceID) == "" || g.StartsAt.IsZero() {
			return fmt.Errorf("declared admin grant requires customer, product, source_id and starts_at")
		}
		if err := db.EnsureCustomerRowQ(ctx, q, merchantID, g.Customer); err != nil {
			return fmt.Errorf("ensure customer %s: %w", g.Customer, err)
		}
		feats, ok := specs[g.Product]
		if !ok {
			product, err := q.GetProductByID(ctx, g.Product)
			if err != nil {
				return fmt.Errorf("import admin grant %s: load product %s: %w", g.SourceID, g.Product, err)
			}
			feats = productEntitlementKeys(product.EntitlementsSpec)
			specs[g.Product] = feats
		}
		if len(feats) == 0 {
			res.Blocked = append(res.Blocked, g.SourceID)
			res.Reasons[g.SourceID] = "product has no entitlements_spec"
			continue
		}
		created, alreadyExists, err := ledger.GrantAdmin(ctx, g.Customer, g.SourceID, feats, g.StartsAt.UTC(), g.EndsAt)
		if err != nil {
			return fmt.Errorf("import admin grant %s: %w", g.SourceID, err)
		}
		switch {
		case alreadyExists:
			res.Skipped = append(res.Skipped, g.SourceID)
		case created > 0:
			res.Imported = append(res.Imported, g.SourceID)
		default:
			res.Blocked = append(res.Blocked, g.SourceID)
			res.Reasons[g.SourceID] = "every feature window overlapped a live window"
		}
	}
	return nil
}

// productEntitlementKeys returns the sorted feature names of a product's
// entitlements_spec ({name: hours} JSONB).
func productEntitlementKeys(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil
	}
	keys := make([]string, 0, len(spec))
	for k := range spec {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
