package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #737: the DeclaredBilling import — a host-declared snapshot of its legacy
// billing book, landed through the SAME decider pipeline the pull/probe/webhook
// planes use. The host asserts FACTS (who, what price, paid-through, explicit
// cancel evidence, dunning evidence, charges); classification of the ambiguous
// cohort is Decide's job, evaluated at the declared AsOf horizon so the same
// book classifies identically no matter when the import runs.
//
// Two lanes per fact:
//   - explicit cancel evidence (user_cancelled / chargeback / provider_terminated)
//     is settled history — written directly with faithful cancel_type and dates;
//     no doctrine is needed to "decide" a fact.
//   - no cancel evidence — seeded as `unknown` (the park state) and resolved by
//     Decide against the declared snapshot: alive-with-future-boundary → adopt,
//     declined/dunning within window → past_due w/ grace, roster-dead → cancel,
//     evidence-starved → stays parked (cancellation-last-resort by construction).

// DeclaredCancelKind is the host's explicit cancel evidence vocabulary.
type DeclaredCancelKind string

const (
	DeclaredCancelNone               DeclaredCancelKind = ""
	DeclaredCancelUser               DeclaredCancelKind = "user_cancelled"
	DeclaredCancelChargeback         DeclaredCancelKind = "chargeback"
	DeclaredCancelProviderTerminated DeclaredCancelKind = "provider_terminated"
)

// DeclaredSubscriptionFact is one subscription's host-declared facts.
type DeclaredSubscriptionFact struct {
	SourceID           string // host's stable id (result reporting)
	Customer           uuid.UUID
	PriceID            uuid.UUID
	Rail               string
	RailSubscriptionID string // required (idempotency key with Rail); hosts synthesize a stable one for rail-less legacy rows
	// PspID is the PSP that owns the declared row. Required (or#893): the
	// import must state which of the merchant's accounts the legacy book came
	// from — there is no unbound lane left to fall into.
	PspID              uuid.UUID
	UserEmail          *string
	StartedAt          time.Time
	PaidThrough        *time.Time // last paid-through evidence (legacy expiration)
	CancelKind         DeclaredCancelKind
	CancelAt           time.Time // required when CancelKind != none
	CancelScheduleLive bool      // provider-side recurring schedule NOT confirmed dead at AsOf
	DunningLive        bool      // legacy dunning schedule still live at AsOf
	DunningRetries     int       // legacy retry count → retry_attempts (forensics)
	DunningLastRetryAt *time.Time
	PaymentMethodID    *uuid.UUID
	Evidence           []byte // verbatim legacy payload → gateway_response at seed
}

// DeclaredOutcome is one fact's import outcome.
type DeclaredOutcome struct {
	Code   string // imported | already_present | blocked
	Reason string
}

const (
	DeclaredImported       = "imported"
	DeclaredAlreadyPresent = "already_present"
	DeclaredBlocked        = "blocked"
)

// declaredImportLookback effectively unbounds the payment backfill: a legacy
// book's charges are all in recoverable scope by declaration (unlike live-pull
// backfill, capped at #634's 3y).
const declaredImportLookback = 200 * 365 * 24 * time.Hour

// DeclaredCoverage is the importer's absence claim, plus the typed confirmation
// that makes the claim expensive to get wrong (or#858).
//
// SubscriptionsExhaustive says "this call is the merchant's ENTIRE book", and
// that is an absence proof: every local subscription NOT in the batch is
// cancelled. An importer that batches its book and forgets to clear the flag
// therefore cancels everything it did not happen to send. A boolean cannot tell
// the two apart — a count can, so the caller must also state how many
// subscriptions the exhaustive book contains, and it must match what arrived.
type DeclaredCoverage struct {
	SubscriptionsExhaustive bool
	// ExpectedSubscriptions is required when SubscriptionsExhaustive. It must
	// equal the number of facts in the call.
	ExpectedSubscriptions *int
}

func (c DeclaredCoverage) validate(got int) error {
	if !c.SubscriptionsExhaustive {
		return nil
	}
	if got == 0 {
		return fmt.Errorf("declared import: refusing an EXHAUSTIVE book with zero subscriptions — that is an absence proof that would cancel every local subscription for this merchant. Send the book, or declare it non-exhaustive")
	}
	if c.ExpectedSubscriptions == nil {
		return fmt.Errorf("declared import: an EXHAUSTIVE book must declare expected_subscriptions (it cancels every local subscription it omits). This call carries %d", got)
	}
	if *c.ExpectedSubscriptions != got {
		return fmt.Errorf("declared import: refusing an EXHAUSTIVE book — expected_subscriptions says %d but the call carries %d. A partial batch declared exhaustive cancels every subscription it omits", *c.ExpectedSubscriptions, got)
	}
	return nil
}

// ImportDeclaredSubscriptions lands one merchant's declared facts. Must run on
// a merchant-scoped connection. asOf is the evidence horizon: Decide evaluates
// at it, and lc's clock should be pinned to it so lifecycle writes are
// deterministic. Idempotent: an existing (rail, rail_subscription_id) row is
// never lifecycle-touched by the direct lane and only moved forward by the
// decider lane; charges land ON CONFLICT DO NOTHING.
func ImportDeclaredSubscriptions(
	ctx context.Context,
	database *db.DB,
	lc *subscriptions.SubscriptionLifecycleService,
	deferDelete subscriptions.DeferredDeleteScheduler,
	merchantID uuid.UUID,
	facts []DeclaredSubscriptionFact,
	txns []RemoteTransaction,
	coverage DeclaredCoverage,
	asOf time.Time,
) (map[string]DeclaredOutcome, error) {
	if database == nil || lc == nil {
		return nil, fmt.Errorf("declared import: db and lifecycle are required")
	}
	if asOf.IsZero() {
		return nil, fmt.Errorf("declared import: AsOf horizon is required")
	}
	if err := coverage.validate(len(facts)); err != nil {
		return nil, err
	}
	out := make(map[string]DeclaredOutcome, len(facts))
	q := database.Gen(ctx)
	repo := subscriptions.NewSubscriptionRepo(database)

	// One declared snapshot for the whole batch: every fact gets a roster entry
	// (cancel facts as dead entries), all declared charges ride Transactions.
	snap := &RemoteSnapshot{
		Provider:     Provider("declared"),
		FetchedAt:    asOf,
		Transactions: txns,
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: coverage.SubscriptionsExhaustive},
	}
	for i := range facts {
		f := &facts[i]
		entry := RemoteSubscription{
			RailSubscriptionID: f.RailSubscriptionID,
			NextBillingAt:      f.PaidThrough,
		}
		switch f.CancelKind {
		case DeclaredCancelNone:
			if f.DunningLive {
				entry.Status = SubscriptionStatusPastDue
			} else {
				entry.Status = SubscriptionStatusActive
			}
		case DeclaredCancelProviderTerminated:
			entry.Status = SubscriptionStatusExpired
		default:
			entry.Status = SubscriptionStatusCancelled
		}
		snap.Subscriptions = append(snap.Subscriptions, entry)
	}

	priceCache := map[uuid.UUID]gen.OpenrailsPrice{}
	productCache := map[uuid.UUID]gen.OpenrailsProduct{}

	for i := range facts {
		f := &facts[i]
		block := func(reason string) { out[f.SourceID] = DeclaredOutcome{Code: DeclaredBlocked, Reason: reason} }
		if f.SourceID == "" {
			return out, fmt.Errorf("declared import: fact %d has no SourceID", i)
		}
		if _, dup := out[f.SourceID]; dup {
			block("duplicate SourceID in batch")
			continue
		}
		if f.RailSubscriptionID == "" || f.Rail == "" {
			block("rail and rail_subscription_id are required (synthesize a stable id for rail-less legacy rows)")
			continue
		}
		if f.Customer == uuid.Nil || f.PriceID == uuid.Nil || f.StartedAt.IsZero() {
			block("customer, price and started_at are required")
			continue
		}
		if f.CancelKind != DeclaredCancelNone && f.CancelAt.IsZero() {
			block("cancel evidence requires its timestamp")
			continue
		}

		price, ok := priceCache[f.PriceID]
		if !ok {
			p, err := q.GetPriceByID(ctx, f.PriceID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					block("price not found")
					continue
				}
				return out, fmt.Errorf("declared import: load price %s: %w", f.PriceID, err)
			}
			price = p
			priceCache[f.PriceID] = p
		}

		// Period pair from facts + catalog: the paid period is one billing cycle
		// wide, its start clamped to signup. This derivation lives HERE (next to
		// the catalog), not in each host's importer.
		var periodStart, periodEnd *time.Time
		if f.PaidThrough != nil && !f.PaidThrough.IsZero() {
			end := f.PaidThrough.UTC()
			start := f.StartedAt.UTC()
			if price.AccessDurationHours != nil && *price.AccessDurationHours > 0 {
				if s := end.Add(-time.Duration(*price.AccessDurationHours) * time.Hour); s.After(start) {
					start = s
				}
			}
			if end.After(start) {
				periodStart, periodEnd = &start, &end
			}
		}

		existing, err := q.GetSubscriptionByRailSubID(ctx, gen.GetSubscriptionByRailSubIDParams{
			Rail:               f.Rail,
			RailSubscriptionID: f.RailSubscriptionID,
		})
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, fmt.Errorf("declared import: lookup %s/%s: %w", f.Rail, f.RailSubscriptionID, err)
		}

		var subID uuid.UUID
		outcome := DeclaredAlreadyPresent
		if exists {
			if existing.CustomerID != f.Customer {
				// The rail key belongs to someone else locally — never silently
				// adopt or converge another customer's row.
				block("rail subscription id already belongs to a different customer")
				continue
			}
			subID = existing.ID
		} else {
			if f.CancelKind != DeclaredCancelNone {
				subID, err = insertDeclaredCancelled(ctx, q, merchantID, f, price, productCache, periodStart, periodEnd)
			} else {
				subID, err = materializeDeclaredUnknown(ctx, q, merchantID, f, periodStart, periodEnd)
			}
			if err != nil {
				block(err.Error())
				continue
			}
			outcome = DeclaredImported
		}

		// Seed-time forensics: verbatim legacy payload + dunning history land on
		// the new row only — existing rows' gateway_response/retry evidence is
		// engine-owned.
		if outcome == DeclaredImported && (len(f.Evidence) > 0 || f.DunningRetries > 0 || f.DunningLastRetryAt != nil) {
			if _, err := database.Qx(ctx).Exec(ctx,
				`UPDATE openrails.subscriptions SET
					gateway_response = COALESCE($1::jsonb, gateway_response),
					retry_attempts = GREATEST(retry_attempts, $2),
					last_retry_at = COALESCE($3, last_retry_at)
				 WHERE id = $4`,
				nilIfEmptyBytes(f.Evidence), f.DunningRetries, f.DunningLastRetryAt, subID); err != nil {
				return out, fmt.Errorf("declared import: stamp evidence for %s: %w", f.SourceID, err)
			}
		}

		// Vault linkage (dunning rebills need subscription→payment_method).
		if f.PaymentMethodID != nil && *f.PaymentMethodID != uuid.Nil {
			if _, err := database.Qx(ctx).Exec(ctx,
				`UPDATE openrails.subscriptions SET payment_method_id = $1 WHERE id = $2 AND payment_method_id IS NULL`,
				*f.PaymentMethodID, subID); err != nil {
				return out, fmt.Errorf("declared import: link payment method for %s: %w", f.SourceID, err)
			}
		}

		sub, err := repo.GetByID(ctx, subID)
		if err != nil {
			return out, fmt.Errorf("declared import: load %s: %w", subID, err)
		}
		if f.CancelKind != DeclaredCancelNone && outcome == DeclaredImported {
			// Fresh row from settled history: written faithfully above; no decider
			// pass, but money truth still lands.
			if _, err := backfillSubscriptionPayments(ctx, q, sub, subscriptionTxns(txns, f.RailSubscriptionID), asOf, declaredImportLookback); err != nil {
				return out, fmt.Errorf("declared import: backfill %s: %w", f.SourceID, err)
			}
			// Declared cancelled but the provider-side schedule was not confirmed
			// dead at AsOf: record the owed remote delete exactly like the runtime
			// producers — DeletionScheduledAt marker + nmi_delete intent in ONE
			// transaction (no crash window, no out-of-band healing needed).
			if f.CancelScheduleLive && deferDelete != nil &&
				rails.RemoteDeleteOnTerminalCancel(models.Rail(f.Rail)) {
				if err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx,
						`UPDATE openrails.subscriptions
						 SET deletion_scheduled_at = $1, updated_at = $1
						 WHERE id = $2 AND deletion_scheduled_at IS NULL`,
						asOf, subID); err != nil {
						return err
					}
					return deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, f.Customer.String(), subID, asOf)
				}); err != nil {
					return out, fmt.Errorf("declared import: schedule remote delete for %s: %w", f.SourceID, err)
				}
			}
		} else {
			// Fresh ambiguous rows AND every pre-existing row (incremental
			// re-import): converge against the declared snapshot at AsOf. A row
			// cancelled between dumps carries a roster-dead entry, so the decider
			// lands the terminal transition (cancel_type 'expired' at AsOf —
			// faithful user/chargeback fidelity applies to first import only);
			// already-terminal rows take no transition but still backfill charges.
			// #835: no first-pull floor here on purpose — the declared snapshot
			// is dated at AsOf and the operator's declaration IS the
			// observation, so AsOf itself is the floor.
			if _, err := convergeSubscriptionFromSnapshotLookback(ctx, database, lc, sub, snap, asOf, 0, declaredImportLookback, time.Time{}); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					// Another live row already owns the customer's lifecycle slot
					// (e.g. uq_subscriptions_customer_product_lifecycle: a twin on
					// a different rail). The row stays parked as seeded — block
					// loudly instead of failing the whole batch.
					block("lifecycle conflict: " + pgErr.ConstraintName)
					continue
				}
				return out, fmt.Errorf("declared import: converge %s: %w", f.SourceID, err)
			}
		}
		out[f.SourceID] = DeclaredOutcome{Code: outcome}
	}
	return out, nil
}

// insertDeclaredCancelled writes an explicitly-cancelled fact directly: settled
// history keeps its faithful cancel_type and dates (the decider's ResolveCancelled
// would stamp 'expired' at AsOf, losing user/chargeback semantics).
func insertDeclaredCancelled(
	ctx context.Context,
	q *gen.Queries,
	merchantID uuid.UUID,
	f *DeclaredSubscriptionFact,
	price gen.OpenrailsPrice,
	productCache map[uuid.UUID]gen.OpenrailsProduct,
	periodStart, periodEnd *time.Time,
) (uuid.UUID, error) {
	product, ok := productCache[price.ProductID]
	if !ok {
		p, err := q.GetProductByID(ctx, price.ProductID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("load product: %w", err)
		}
		product = p
		productCache[price.ProductID] = p
	}

	var cancelType string
	switch f.CancelKind {
	case DeclaredCancelUser:
		cancelType = string(models.CancelTypeUser)
	case DeclaredCancelChargeback:
		cancelType = string(models.CancelTypeChargeback)
	case DeclaredCancelProviderTerminated:
		cancelType = string(models.CancelTypeExpired)
	default:
		return uuid.Nil, fmt.Errorf("unsupported cancel kind %q", f.CancelKind)
	}

	cancelledAt := f.CancelAt.UTC()
	// Cancel-with-runway keeps the paid-through end; otherwise access ended at the cancel.
	endedAt := cancelledAt
	if f.PaidThrough != nil && f.PaidThrough.After(cancelledAt) {
		endedAt = f.PaidThrough.UTC()
	}
	feedback := "imported: declared " + string(f.CancelKind)

	id := uuid.New()
	priceID := f.PriceID
	if _, err := q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID:                       id,
		MerchantID:               merchantID,
		CustomerID:               f.Customer,
		ProductID:                price.ProductID,
		PriceID:                  &priceID,
		EntitlementsSpecSnapshot: product.EntitlementsSpec,
		CreditsSpecSnapshot:      product.CreditsSpec,
		Status:                   string(models.StatusCancelled),
		StartedAt:                f.StartedAt.UTC(),
		EndedAt:                  &endedAt,
		CurrentPeriodStartsAt:    periodStart,
		CurrentPeriodEndsAt:      periodEnd,
		Rail:                     f.Rail,
		RailSubscriptionID:       f.RailSubscriptionID,
		UserEmail:                f.UserEmail,
		CancelFeedback:           &feedback,
		CancelType:               &cancelType,
		CancelledAt:              &cancelledAt,
		CreatedAt:                f.StartedAt.UTC(),
		UpdatedAt:                cancelledAt,
		PspID:                    f.PspID,
	}); err != nil {
		// A race with a concurrent writer trips the (merchant, rail, sub-id)
		// unique index — a loud blocked row, never silent corruption.
		return uuid.Nil, fmt.Errorf("insert cancelled subscription: %w", err)
	}
	return id, nil
}

// materializeDeclaredUnknown seeds the ambiguous lane through the pull path's
// own PS-1 materialization: spec snapshots from the catalog, idempotent by
// (rail, rail_subscription_id), born `unknown` for the decider to resolve.
func materializeDeclaredUnknown(
	ctx context.Context,
	q *gen.Queries,
	merchantID uuid.UUID,
	f *DeclaredSubscriptionFact,
	periodStart, periodEnd *time.Time,
) (uuid.UUID, error) {
	started := f.StartedAt.UTC()
	rows, err := q.ReconcileMaterializeSubscription(ctx, gen.ReconcileMaterializeSubscriptionParams{
		MerchantID:         merchantID,
		Status:             gen.OpenrailsSubscriptionStatus(models.StatusUnknown),
		Rail:               f.Rail,
		RailSubscriptionID: f.RailSubscriptionID,
		UserEmail:          f.UserEmail,
		PeriodStartsAt:     periodStart,
		PeriodEndsAt:       periodEnd,
		StartedAt:          &started,
		CustomerID:         f.Customer,
		PspID:              f.PspID,
		PriceID:            f.PriceID,
		Rails:              []string{f.Rail},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("materialize subscription: %w", err)
	}
	if len(rows) == 0 {
		// Raced an insert between lookup and materialize; re-read.
		existing, err := q.GetSubscriptionByRailSubID(ctx, gen.GetSubscriptionByRailSubIDParams{
			Rail:               f.Rail,
			RailSubscriptionID: f.RailSubscriptionID,
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("materialize raced but row not found: %w", err)
		}
		return existing.ID, nil
	}
	return rows[0].ID, nil
}

func nilIfEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// subscriptionTxns filters the batch transactions to one subscription.
func subscriptionTxns(txns []RemoteTransaction, railSubID string) []RemoteTransaction {
	var out []RemoteTransaction
	for i := range txns {
		if txns[i].SubscriptionID == railSubID {
			out = append(out, txns[i])
		}
	}
	return out
}
