// Package billingimport imports a host's legacy billing book as facts and
// classifies them through the same decider the pull, probe and webhook paths
// use, at the declared AsOf. Admin comps ride the same book. The wire
// vocabulary (billing.DeclaredBilling, POST /v1/admin/billing-import) is
// aliased here.
package billingimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// The declared-facts vocabulary is the shared Client wire (billing.DeclaredBilling);
// these aliases keep the engine-side names.
type (
	DeclaredCustomer      = billing.DeclaredCustomer
	PSPRef                = billing.PSPRef
	DeclaredPaymentMethod = billing.DeclaredPaymentMethod
	PaymentMethodRef      = billing.PaymentMethodRef
	CancelEvidence        = billing.CancelEvidence
	DunningEvidence       = billing.DunningEvidence
	DeclaredTransaction   = billing.DeclaredTransaction
	DeclaredSubscription  = billing.DeclaredSubscription
	DeclaredAdminGrant    = billing.DeclaredAdminGrant
	DeclaredBilling       = billing.DeclaredBilling
	Result                = billing.BillingImportResult
)

// Options borrows the runtime database; Import never opens or closes
// resources. MerchantID is already resolved: imports never interpret a public
// name.
type Options struct {
	DB         *db.DB
	MerchantID billing.MerchantID
	Book       DeclaredBilling
	// Clock is the runtime clock the post-import derivation evaluates at.
	Clock clockwork.Clock
}

// Import lands a host-declared billing book. Explicitly canceled facts are
// written directly; the ambiguous cohort is seeded `unknown` and resolved by
// the decider against the declared snapshot at AsOf. Charges land idempotently
// by (rail, transaction_id). One merchant-scoped transaction: infrastructure
// failures roll back the whole book; per-source business refusals stay
// ordinary outcomes for the other rows.
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

		// Lifecycle clock pinned at AsOf so re-runs are deterministic. The
		// service and deferred-delete scheduler share this transaction so no
		// write partially commits.
		lc := subscriptions.NewSubscriptionLifecycleService(txdb, nil, nil, nil, nil, nil, clockwork.NewFakeClockAt(asOf))
		deferDelete := intents.NewProviderCancelScheduler(txdb, nil, intents.OriginUser, "billing-import terminal cancel, remote may be alive")
		lc.SetProviderCancelScheduler(deferDelete)

		// Every provider-bound row carries a PSP: its own ref, else the book
		// default.
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
		// declaredAgreements: a card's declared recurring agreement (the storing
		// transaction of its NMI schedules), which their mandates cite.
		declaredAgreements := map[uuid.UUID]string{}
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
			existing, err := q.GetPaymentMethodByPSPRefs(ctx, gen.GetPaymentMethodByPSPRefsParams{
				MerchantID: merchantID.UUID(), PspID: pmPSP, RailCustomerRef: pm.RailCustomerRef, RailMethodRef: pm.RailMethodRef,
			})
			id, owner, existingRail := existing.ID, existing.CustomerID, existing.Rail
			if errors.Is(err, pgx.ErrNoRows) {
				id = uuid.New()
				created := pm.CreatedAt.UTC()
				if created.IsZero() {
					created = asOf
				}
				brand, last4, month, year := models.CardFromDetails(pm.Card).Columns()
				if _, err := q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
					ID:              id,
					MerchantID:      merchantID.UUID(),
					CustomerID:      pm.Customer.UUID(),
					Rail:            pm.Rail,
					RailCustomerRef: pm.RailCustomerRef,
					RailMethodRef:   pm.RailMethodRef,
					PspID:           &pmPSP,
					Custodian:       models.CustodianPSP,
					CardBrand:       brand,
					CardLast4:       last4,
					CardExpMonth:    month,
					CardExpYear:     year,
					CreatedAt:       created,
					UpdatedAt:       created,
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
				declaredAgreements[id] = pm.RecurringTransactionID
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
			minor, err := moneyutil.NativeToRailMinorExact(t.Currency, t.Amount)
			if err != nil {
				return apperr.Invalidf("transaction %s amount: %v", t.TransactionID, err).WithParam("transactions")
			}
			txns = append(txns, reconcile.RemoteTransaction{
				TransactionID:  t.TransactionID,
				SubscriptionID: t.RailSubscriptionID,
				Type:           typ,
				Success:        t.Success,
				AmountCents:    int64(minor),
				Currency:       t.Currency,
				OccurredAt:     t.OccurredAt.UTC(),
			})
		}

		// The first approved sale of each NMI schedule is its customer-initiated
		// recurring signup: the stored-credential anchor dunning retries cite.
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
		anchors := map[string]string{}
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
					row, err := q.GetPaymentMethodByPSPRailRefs(ctx, gen.GetPaymentMethodByPSPRailRefsParams{
						MerchantID: merchantID.UUID(), PspID: subPSP, Rail: s.PaymentMethod.Rail,
						RailCustomerRef: s.PaymentMethod.RailCustomerRef, RailMethodRef: s.PaymentMethod.RailMethodRef,
					})
					existing, owner := row.ID, row.CustomerID
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
			anchor, err := dunNMISchedule(ctx, q, merchantID.UUID(), &f, declaredAgreements, firstScheduleSale)
			if err != nil {
				return err
			}
			if anchor != "" {
				anchors[f.SourceID] = anchor
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
		for _, f := range facts {
			o := outcomes[f.SourceID]
			if anchor := anchors[f.SourceID]; anchor != "" && (o.Code == reconcile.DeclaredImported || o.Code == reconcile.DeclaredAlreadyPresent) {
				if err := importRecurringMandate(ctx, q, merchantID.UUID(), f, anchor, asOf); err != nil {
					return err
				}
			}
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
func deriveImportedAccess(ctx context.Context, q *gen.Queries, merchantID billing.MerchantID, book DeclaredBilling, clock clockwork.Clock) error {
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

// importAdminGrants records each comp as a free grant of its product,
// idempotent by SourceID.
func importAdminGrants(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, declared []DeclaredAdminGrant, res *Result) error {
	if len(declared) == 0 {
		return nil
	}
	ledger := grants.New(q, merchantID)
	for _, g := range declared {
		if g.Customer.IsZero() || g.Product.IsZero() || strings.TrimSpace(g.SourceID) == "" || g.StartsAt.IsZero() {
			return apperr.Invalidf("declared admin grant requires customer, product, source_id and starts_at")
		}
		if err := db.EnsureCustomerRowQ(ctx, q, merchantID, g.Customer.UUID()); err != nil {
			return fmt.Errorf("ensure customer %s: %w", g.Customer, err)
		}
		_, created, err := ledger.GrantProduct(ctx, g.Customer.UUID(), g.Product.UUID(), g.SourceID, g.StartsAt.UTC(), g.EndsAt, "import", grants.ReasonImport, nil)
		if err != nil {
			return fmt.Errorf("import admin grant %s: %w", g.SourceID, err)
		}
		if created {
			res.Imported = append(res.Imported, g.SourceID)
		} else {
			res.Skipped = append(res.Skipped, g.SourceID)
		}
	}
	return nil
}

// FindingNoRecurringAnchor is an imported NMI schedule OpenRails cannot dun:
// its card has no recurring stored-credential anchor.
const FindingNoRecurringAnchor = "life.import.no_recurring_anchor"

// dunNMISchedule finds the recurring anchor OpenRails dunning charges an
// imported NMI schedule's retries against (NMI never retries): the card's
// declared agreement, else the one the schedule's mandate already holds, else
// the schedule's first approved sale (its recurring signup), else the card's
// recurring lineage on the schedule's account. Without one an operator
// finding names the schedule OpenRails cannot retry.
func dunNMISchedule(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, f *reconcile.DeclaredSubscriptionFact, declared map[uuid.UUID]string, firstSale map[string]reconcile.RemoteTransaction) (string, error) {
	if !strings.EqualFold(f.Rail, "nmi") || f.RailSubscriptionID == "" || f.CancelKind != reconcile.DeclaredCancelNone {
		return "", nil
	}
	if f.PaymentMethodID != nil {
		if anchor := declared[*f.PaymentMethodID]; anchor != "" {
			return anchor, nil
		}
		sub, err := q.GetSubscriptionByPSPSubID(ctx, gen.GetSubscriptionByPSPSubIDParams{MerchantID: merchantID, PspID: f.PspID, Rail: f.Rail, RailSubscriptionID: f.RailSubscriptionID})
		if err == nil && sub.PaymentMethodID != nil && *sub.PaymentMethodID == *f.PaymentMethodID {
			if lineage, err := mandates.ForSubscription(ctx, q, merchantID, f.Customer, sub.ID, *f.PaymentMethodID, f.PspID); err == nil {
				return lineage.InitialTransactionID, nil
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("load imported schedule %s: %w", f.SourceID, err)
		}
		if sale, ok := firstSale[f.RailSubscriptionID]; ok {
			return sale.TransactionID, nil
		}
		lineage, err := mandates.Citable(ctx, q, merchantID, f.Customer, *f.PaymentMethodID, f.PspID, f.Rail, charge.AgreementRecurring)
		if err != nil {
			return "", fmt.Errorf("load recurring lineage for %s: %w", f.SourceID, err)
		}
		if lineage != nil {
			return lineage.InitialTransactionID, nil
		}
	}
	evidence, _ := json.Marshal(map[string]any{"source_id": f.SourceID, "rail_subscription_id": f.RailSubscriptionID})
	action := fmt.Sprintf("imported NMI schedule %s has no recurring card agreement (no recurring_transaction_id and no approved sale for the schedule), so OpenRails cannot retry its declines. Import the schedule's first approved sale or the card's recurring agreement", f.RailSubscriptionID)
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: merchantID, FindingType: FindingNoRecurringAnchor, SubjectKey: f.RailSubscriptionID,
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return "", err
}

// importRecurringMandate gives an imported schedule the recurring mandate that
// cites its anchor. A reimport never replaces an accepted agreement.
func importRecurringMandate(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, f reconcile.DeclaredSubscriptionFact, anchor string, at time.Time) error {
	sub, err := q.GetSubscriptionByPSPSubID(ctx, gen.GetSubscriptionByPSPSubIDParams{MerchantID: merchantID, PspID: f.PspID, Rail: f.Rail, RailSubscriptionID: f.RailSubscriptionID})
	if err != nil {
		return fmt.Errorf("load imported schedule %s: %w", f.SourceID, err)
	}
	if sub.PaymentMethodID == nil || f.PaymentMethodID == nil || *sub.PaymentMethodID != *f.PaymentMethodID {
		return nil
	}
	live, err := q.GetLiveRecurringMandateForShare(ctx, gen.GetLiveRecurringMandateForShareParams{MerchantID: merchantID, SubscriptionID: sub.ID})
	switch {
	case err == nil:
		if live.InitialTransactionID == nil {
			return mandates.SetLineage(ctx, q, merchantID, live.ID, charge.Mandate{InitialTransactionID: anchor})
		}
		if *live.InitialTransactionID != anchor {
			return apperr.Conflictf("schedule %s already has a different recurring agreement", f.SourceID)
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	_, err = mandates.Create(ctx, q, mandates.Agreement{MerchantID: merchantID, CustomerID: sub.CustomerID, PaymentMethodID: *sub.PaymentMethodID, PSPID: sub.PspID, Rail: sub.Rail,
		Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: &charge.Mandate{InitialTransactionID: anchor}, AcceptedAt: at})
	return err
}
