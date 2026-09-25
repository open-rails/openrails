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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
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

// Options retains the runtime database and its bound River producer. Import
// borrows them and never opens or closes resources. MerchantID is the already resolved or authorized
// immutable merchant UUID; imports never interpret a public name.
type Options struct {
	DB         *db.DB
	MerchantID merchant.ID
	Book       DeclaredBilling
	// Clock is the runtime clock the post-import derivation evaluates at.
	Clock clockwork.Clock
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
		return res, apperr.Invalidf("import billing: Book.AsOf (evidence horizon) is required")
	}
	if err := rejectDeclaredPANs(opts.Book); err != nil {
		return res, err
	}
	asOf := opts.Book.AsOf.UTC()

	database := opts.DB
	if database == nil {
		return res, fmt.Errorf("billing import requires the runtime database")
	}

	merchantID := opts.MerchantID
	if err := database.RequireMerchantID(ctx, merchantID); err != nil {
		return res, err
	}
	ctx = merchant.WithID(ctx, merchantID)

	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		qx := txdb.Qx(ctx)
		q := txdb.Gen(ctx)

		// Lifecycle clock pinned at AsOf: every lifecycle write (ended_at, grace,
		// updated_at) is dated at the horizon — deterministic re-runs. The service
		// and deferred-delete scheduler share this transaction so subscription,
		// evidence, payment and intent writes cannot partially commit.
		lc := subscriptions.NewSubscriptionLifecycleService(txdb, nil, nil, nil, nil, nil, clockwork.NewFakeClockAt(asOf))
		deferDelete := intents.NewProviderCancelScheduler(txdb, nil, intents.OriginUser, "billing-import terminal cancel, remote may be alive")
		lc.SetProviderCancelScheduler(deferDelete)

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
			if c.Customer.IsZero() {
				continue
			}
			if err := db.EnsureCustomerRow(ctx, qx, merchantID.UUID(), c.Customer.UUID()); err != nil {
				return fmt.Errorf("ensure customer %s: %w", c.Customer, err)
			}
			seen[c.Customer.UUID()] = struct{}{}
		}
		for _, s := range opts.Book.Subscriptions {
			if s.Customer.IsZero() {
				continue
			}
			if _, ok := seen[s.Customer.UUID()]; ok {
				continue
			}
			if err := db.EnsureCustomerRow(ctx, qx, merchantID.UUID(), s.Customer.UUID()); err != nil {
				return fmt.Errorf("ensure customer %s: %w", s.Customer, err)
			}
			seen[s.Customer.UUID()] = struct{}{}
		}

		// Payment methods: idempotent by the PSP-scoped instrument identity. An
		// existing instrument is reusable only by the same customer and rail; the
		// assertion lives in this transaction so hosts do not need a racy precheck.
		pmIDs := map[string]uuid.UUID{}
		pmKey := func(psp uuid.UUID, rail, custRef, methodRef string) string {
			return psp.String() + "\x1f" + rail + "\x1f" + custRef + "\x1f" + methodRef
		}
		for _, pm := range opts.Book.PaymentMethods {
			if pm.Rail == "" || pm.RailCustomerRef == "" || pm.Customer.IsZero() {
				return apperr.Invalidf("declared payment method requires rail, rail_customer_ref and customer")
			}
			if pm.RecurringTransactionID != "" && (!strings.EqualFold(pm.Rail, "nmi") || strings.TrimSpace(pm.RecurringTransactionID) != pm.RecurringTransactionID) {
				return apperr.Invalidf("recurring_transaction_id requires NMI and an unpadded transaction reference")
			}
			pmPSP, err := psps.resolve(pm.PSP, pm.Rail, fmt.Sprintf("payment method %s/%s", pm.Rail, pm.RailCustomerRef))
			if err != nil {
				return err
			}
			if strings.EqualFold(strings.TrimSpace(pm.Rail), "nmi") {
				if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID.UUID(), ID: pm.Customer.UUID()}); err != nil {
					return err
				}
				if err := paymentmethods.LockNativeVault(ctx, q, merchantID.UUID(), pmPSP, pm.RailCustomerRef); err != nil {
					return err
				}
				if err := paymentmethods.RequireNativeVaultAvailable(ctx, q, merchantID.UUID(), pmPSP, pm.RailCustomerRef, pm.RailMethodRef); err != nil {
					return err
				}
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
					CustomerID:           pm.Customer.UUID(),
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
				}); err != nil {
					return fmt.Errorf("create payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
				}
			} else if err != nil {
				return fmt.Errorf("lookup payment method %s/%s: %w", pm.Rail, pm.RailCustomerRef, err)
			} else if owner != pm.Customer.UUID() {
				return apperr.Conflictf("payment method %s/%s already belongs to customer %s, not %s", pm.Rail, pm.RailCustomerRef, owner, pm.Customer)
			} else if !strings.EqualFold(existingRail, pm.Rail) {
				return apperr.Conflictf("payment method %s/%s is stored on rail %q, not %q", pm.Rail, pm.RailCustomerRef, existingRail, pm.Rail)
			}
			if pm.RecurringTransactionID != "" {
				_, err := q.CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
					MerchantID: merchantID.UUID(), ID: id, Agreement: "recurring", Ref: pm.RecurringTransactionID,
				})
				if err != nil {
					return fmt.Errorf("import recurring agreement: %w", err)
				}
				stored, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID.UUID(), ID: id})
				if err != nil {
					return err
				}
				if stored.StoredCredentialRecurringRef != pm.RecurringTransactionID {
					return apperr.Conflictf("payment method already has a different recurring agreement")
				}
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

		// The first approved sale of each NMI schedule is its customer-initiated
		// recurring signup: the stored-credential anchor OpenRails' dunning
		// retries reference (Paul, 2026-09-25: every NMI schedule is dunned).
		firstScheduleSale := map[string]reconcile.RemoteTransaction{}
		for _, t := range txns {
			if t.SubscriptionID == "" || !t.Success || t.Type != reconcile.TransactionTypeSale || t.TransactionID == "" {
				continue
			}
			if prior, seen := firstScheduleSale[t.SubscriptionID]; !seen || t.OccurredAt.Before(prior.OccurredAt) {
				firstScheduleSale[t.SubscriptionID] = t
			}
		}

		facts := make([]reconcile.DeclaredSubscriptionFact, 0, len(opts.Book.Subscriptions))
		for _, s := range opts.Book.Subscriptions {
			subPSP, err := psps.resolve(s.PSP, s.Rail, fmt.Sprintf("subscription %s", s.SourceID))
			if err != nil {
				return err
			}
			f := reconcile.DeclaredSubscriptionFact{
				SourceID:           s.SourceID,
				Customer:           s.Customer.UUID(),
				PriceID:            s.Price.UUID(),
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
						if owner != s.Customer.UUID() {
							return apperr.Conflictf("resolve payment method ref %s: instrument belongs to customer %s, not %s", s.SourceID, owner, s.Customer)
						}
						id, ok = existing, true
						pmIDs[key] = existing
					} else if err != pgx.ErrNoRows {
						return fmt.Errorf("resolve payment method ref %s: %w", s.SourceID, err)
					}
				}
				if ok {
					f.PaymentMethodID = &id
				} else {
					f.PaymentMethodUnresolved = true
				}
			}
			if err := dunNMISchedule(ctx, q, merchantID.UUID(), &f, firstScheduleSale); err != nil {
				return err
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
		// Access commits with the memberships it derives from: a converge
		// running between the two would otherwise see a member without a
		// grant and record the import's own effect as a repair.
		if err := deriveImportedAccess(ctx, q, merchantID, opts.Book, opts.Clock); err != nil {
			return fmt.Errorf("derive imported access: %w", err)
		}
		sort.Strings(res.Imported)
		sort.Strings(res.Skipped)
		sort.Strings(res.Blocked)
		return nil
	})
	if err != nil {
		return res, err
	}
	if err := failpoint.Hit(ctx, failpoint.Site{Point: failpoint.Committed, Kind: FailpointKind}); err != nil {
		return res, err
	}
	return res, nil
}

// FailpointKind names the import at its failpoints.
const FailpointKind = "billing_import"

// deriveImportedAccess derives, for exactly the book's customers, the grants
// an operator convergence would: an imported member is entitled without a
// merchant-wide pass.
func deriveImportedAccess(ctx context.Context, q *gen.Queries, merchantID merchant.ID, book DeclaredBilling, clock clockwork.Clock) error {
	customers := map[uuid.UUID]struct{}{}
	for _, c := range book.Customers {
		customers[c.Customer.UUID()] = struct{}{}
	}
	for _, s := range book.Subscriptions {
		customers[s.Customer.UUID()] = struct{}{}
	}
	for _, g := range book.AdminGrants {
		customers[g.Customer.UUID()] = struct{}{}
	}
	delete(customers, uuid.Nil)
	ids := make([]uuid.UUID, 0, len(customers))
	for id := range customers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	now := timeutil.FirstClock(clock).Now().UTC()
	scanSince := now.Add(-3 * 365 * 24 * time.Hour)
	ledger := grants.New(q, merchantID.UUID())
	for i := range ids {
		subs, err := ledger.UngrantedSubscriptions(ctx, &ids[i], scanSince)
		if err != nil {
			return fmt.Errorf("customer %s: %w", ids[i], err)
		}
		for _, sub := range subs {
			if err := ledger.DeriveSubscriptionGrant(ctx, sub); err != nil {
				return fmt.Errorf("customer %s subscription %s: %w", ids[i], sub.ID, err)
			}
		}
	}
	return nil
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
		if g.Customer.IsZero() || g.Product.IsZero() || strings.TrimSpace(g.SourceID) == "" || g.StartsAt.IsZero() {
			return apperr.Invalidf("declared admin grant requires customer, product, source_id and starts_at")
		}
		if err := db.EnsureCustomerRowQ(ctx, q, merchantID, g.Customer.UUID()); err != nil {
			return fmt.Errorf("ensure customer %s: %w", g.Customer, err)
		}
		feats, ok := specs[g.Product.UUID()]
		if !ok {
			product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{ID: g.Product.UUID(), MerchantID: merchantID})
			if err != nil {
				return fmt.Errorf("import admin grant %s: load product %s: %w", g.SourceID, g.Product, err)
			}
			feats = productEntitlementKeys(product.EntitlementsSpec)
			specs[g.Product.UUID()] = feats
		}
		if len(feats) == 0 {
			res.Blocked = append(res.Blocked, g.SourceID)
			res.Reasons[g.SourceID] = "product has no entitlements_spec"
			continue
		}
		created, alreadyExists, err := ledger.GrantAdmin(ctx, g.Customer.UUID(), g.SourceID, feats, g.StartsAt.UTC(), g.EndsAt)
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

// FindingNoRecurringAnchor is an imported NMI schedule OpenRails cannot dun:
// its card has no recurring stored-credential anchor.
const FindingNoRecurringAnchor = "life.import.no_recurring_anchor"

// dunNMISchedule records the recurring anchor OpenRails dunning charges an
// imported NMI schedule's retries against (NMI never retries). A card without
// one takes the schedule's first approved sale (its recurring signup); without
// that an operator finding names the schedule OpenRails cannot retry.
func dunNMISchedule(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, f *reconcile.DeclaredSubscriptionFact, firstSale map[string]reconcile.RemoteTransaction) error {
	if !strings.EqualFold(f.Rail, "nmi") || f.RailSubscriptionID == "" || f.CancelKind != reconcile.DeclaredCancelNone {
		return nil
	}
	if f.PaymentMethodID != nil {
		method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: *f.PaymentMethodID})
		if err != nil {
			return fmt.Errorf("load payment method for %s: %w", f.SourceID, err)
		}
		anchor := method.StoredCredentialRecurringRef
		if sale, ok := firstSale[f.RailSubscriptionID]; anchor == "" && ok {
			if _, err := q.CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: merchantID, ID: method.ID, Agreement: "recurring", Ref: sale.TransactionID}); err != nil {
				return fmt.Errorf("import recurring anchor for %s: %w", f.SourceID, err)
			}
			anchor = sale.TransactionID
		}
		if anchor != "" {
			return nil
		}
	}
	evidence, _ := json.Marshal(map[string]any{"source_id": f.SourceID, "rail_subscription_id": f.RailSubscriptionID})
	action := fmt.Sprintf("imported NMI schedule %s has no recurring card agreement (no recurring_transaction_id and no approved sale for the schedule), so OpenRails cannot retry its declines. Import the schedule's first approved sale or the card's recurring agreement", f.RailSubscriptionID)
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: merchantID, FindingType: FindingNoRecurringAnchor, SubjectKey: f.RailSubscriptionID,
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}
