package subscriptions

// #813 (implements #778's sketch): operator-driven bulk plan migration —
// "retire plan-A, move every subscriber to plan-B" — built as the
// cross-product generalization of #773's reprice engine rather than a second
// workflow. One scheduled-change primitive (subscription_reprices) carries
// both kinds; the ONLY new machinery here is cohort enumeration, per-rail
// capability classification, the rail push for observed rails (Stripe), and
// the batch ledger.
//
// Money semantics (the forced-migration invariant): NEVER surprise-charge.
// No proration, no cycle reset, in every mode. The flip lands at the
// subscription's first renewal on/after the effective date; Immediate
// additionally cuts entitlements over right away (billing still flips at the
// next invoice).
//
// Rail capability matrix (classified per subscription, surfaced by Preview
// BEFORE the operator commits):
//   - stripe:            AUTO — push the price change to Stripe (boundary =
//                        subscription schedule at period end; immediate =
//                        item update with proration_behavior=none); converge
//                        applies the internal cutover from provider truth.
//   - engine-driven      AUTO — no rail push needed; the #773 renewal-boundary
//     (no rail sub id):  pickup (ResolveEffectivePrice / RenewMembership)
//                        applies it when OpenRails initiates the charge.
//   - ccbill, solana,    REQUIRES USER ACTION — the rail cannot be mutated
//     native gateway     server-side (redirect flows / on-chain co-sign /
//     recurring:         gateway-owned plans). Rows land status=blocked with
//                        the reason; the operator's fallback_policy records
//                        intent (keep_grandfathered | cancel_at_period_end —
//                        the latter is NOT automated in v1, it is surfaced).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Plan-migration classification reasons (stable strings — they land in
// subscription_reprices.blocked_reason and in preview/batch responses).
const (
	migrationBlockedRailUserAction   = "rail_requires_user_action"
	migrationBlockedMissingRailPrice = "target_price_missing_rail_config"
	migrationBlockedRailPushFailed   = "rail_push_failed"
)

var (
	// ErrPlanMigrationSameProduct: source and target price resolve to the same
	// product — that is a #773 reprice, not a plan migration.
	ErrPlanMigrationSameProduct = errors.New("plan migration: target price is on the source product (use reprice)")
	// ErrPlanMigrationBadFallback: fallback_policy outside the vocabulary.
	ErrPlanMigrationBadFallback = errors.New("plan migration: fallback_policy must be keep_grandfathered or cancel_at_period_end")
)

// PlanMigrationRequest is one operator decision: retire source_price, move
// its whole cohort to target_price.
type PlanMigrationRequest struct {
	SourcePriceID uuid.UUID
	TargetPriceID uuid.UUID
	// EffectiveAt: each subscription flips at its FIRST RENEWAL on/after this
	// instant (rolling per-sub — never mid-cycle, never a surprise charge).
	// Zero value means "now" (= each sub's next renewal).
	EffectiveAt time.Time
	// Immediate additionally applies the entitlement/product cutover right
	// away for auto-migratable subscriptions (billing still flips at the next
	// invoice; nothing is charged now).
	Immediate bool
	// AcknowledgeShortNotice (#781): explicit override when the migration is a
	// price INCREASE inside the merchant's configured notice window.
	AcknowledgeShortNotice bool
	// FallbackPolicy records the operator's intent for subscriptions on rails
	// that cannot be auto-migrated. Default keep_grandfathered.
	FallbackPolicy string
	// ArchiveSource: archive the source price (stop new purchases) as part of
	// the migration. Default true; preview ignores it.
	ArchiveSource *bool
}

// PlanMigrationOutcome is one subscription's classification in a migration
// (or preview).
type PlanMigrationOutcome struct {
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	RepriceID      *uuid.UUID `json:"reprice_id,omitempty"`
	Rail           string     `json:"rail"`
	// Disposition: scheduled | applied_immediately | skipped | blocked.
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

// PlanMigrationResult is the batch header + per-subscription ledger returned
// by both Preview (BatchID nil, nothing written) and Migrate.
type PlanMigrationResult struct {
	BatchID        *uuid.UUID             `json:"batch_id,omitempty"`
	SourcePriceID  uuid.UUID              `json:"source_price_id"`
	TargetPriceID  uuid.UUID              `json:"target_price_id"`
	EffectiveAt    time.Time              `json:"effective_at"`
	FallbackPolicy string                 `json:"fallback_policy"`
	Matched        int                    `json:"matched"`
	Scheduled      int                    `json:"scheduled"`
	Skipped        int                    `json:"skipped"`
	Blocked        int                    `json:"blocked"`
	ByRail         map[string]*RailCounts `json:"by_rail"`
	Outcomes       []PlanMigrationOutcome `json:"outcomes"`
	SourceArchived bool                   `json:"source_archived"`
}

// RailCounts is the per-rail capability summary the operator reviews before
// committing (auto-migratable vs requires-action vs blocked/skipped).
type RailCounts struct {
	Auto           int `json:"auto"`
	RequiresAction int `json:"requires_action"`
	Skipped        int `json:"skipped"`
}

// PaymentMethodLookup resolves a subscription's payment method — the
// engine-driven-rail detector (#297 stored-credential recurring anchor).
type PaymentMethodLookup interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error)
}

// StripePusher is the observed-rail push seam (satisfied by
// *StripeService). Extracted as an interface so migration tests can run
// against a fake without a live Stripe.
type StripePusher interface {
	GetSubscriptionItemID(ctx context.Context, subscriptionID string) (string, error)
	UpdateSubscriptionPrice(ctx context.Context, subscriptionID, itemID, newPriceID, internalPriceID, prorationBehavior, billingAnchor string) error
	ScheduleSubscriptionPriceChange(ctx context.Context, subscriptionID, currentPriceID, newPriceID string, currentPeriodStart, currentPeriodEnd time.Time, billingCycleDays *int) (string, error)
}

// PlanMigrationService drives #813 bulk plan migrations over the #773
// reprice engine.
type PlanMigrationService struct {
	reprice        *RepriceService
	stripe         StripePusher
	paymentMethods PaymentMethodLookup
}

func NewPlanMigrationService(reprice *RepriceService, stripe StripePusher, paymentMethods PaymentMethodLookup) *PlanMigrationService {
	return &PlanMigrationService{reprice: reprice, stripe: stripe, paymentMethods: paymentMethods}
}

// migrationCapability classifies one subscription's rail for forced
// server-side migration.
type migrationCapability int

const (
	capabilityAutoInternal migrationCapability = iota // engine-driven: reprice row alone suffices
	capabilityAutoStripe                              // push to Stripe, converge applies
	capabilityUserAction                              // rail cannot be mutated server-side
)

func (s *PlanMigrationService) classifyMigrationCapability(ctx context.Context, sub *models.Subscription) migrationCapability {
	switch {
	case sub.Rail == models.RailStripe:
		return capabilityAutoStripe
	case sub.Rail == models.RailCCBill, sub.Rail == models.RailSolana:
		return capabilityUserAction
	default:
		// Non-stripe card rails split by who INITIATES the recurring charge:
		// a payment method carrying the #297 stored-credential recurring
		// anchor means OpenRails' own engine charges (RenewalCharger) — the
		// renewal-boundary pickup is then the whole mechanism. Anything else
		// is a gateway-native recurring plan OpenRails only observes; the
		// gateway would keep charging the old plan, so it cannot be
		// auto-migrated server-side.
		if s.paymentMethods == nil || sub.PaymentMethodID == nil {
			return capabilityUserAction
		}
		pm, err := s.paymentMethods.GetByID(ctx, *sub.PaymentMethodID)
		if err != nil || pm == nil || strings.TrimSpace(pm.StoredCredentialRecurringRef) == "" {
			return capabilityUserAction
		}
		return capabilityAutoInternal
	}
}

func normalizeFallback(policy string) (string, error) {
	switch strings.TrimSpace(policy) {
	case "", models.MigrationFallbackKeepGrandfathered:
		return models.MigrationFallbackKeepGrandfathered, nil
	case models.MigrationFallbackCancelAtPeriodEnd:
		return models.MigrationFallbackCancelAtPeriodEnd, nil
	default:
		return "", ErrPlanMigrationBadFallback
	}
}

// resolveMigration loads and validates the source/target pair.
func (s *PlanMigrationService) resolveMigration(ctx context.Context, req *PlanMigrationRequest) (source, target *models.Price, err error) {
	source, err = s.reprice.prices.GetByID(ctx, req.SourcePriceID)
	if err != nil {
		return nil, nil, fmt.Errorf("plan migration: source price: %w", err)
	}
	target, err = s.reprice.prices.GetByID(ctx, req.TargetPriceID)
	if err != nil {
		return nil, nil, fmt.Errorf("plan migration: target price: %w", err)
	}
	if target.Archived {
		return nil, nil, &RepriceConstraintError{Sentinel: ErrRepriceInactivePrice, FromPriceID: source.ID, ToPriceID: target.ID}
	}
	if target.ProductID == source.ProductID {
		return nil, nil, ErrPlanMigrationSameProduct
	}
	if !strings.EqualFold(strings.TrimSpace(target.Currency), strings.TrimSpace(source.Currency)) {
		return nil, nil, &RepriceConstraintError{Sentinel: ErrRepriceCrossCurrency, FromPriceID: source.ID, ToPriceID: target.ID}
	}
	if req.EffectiveAt.IsZero() {
		req.EffectiveAt = s.reprice.now()
	}
	return source, target, nil
}

// classify runs the shared per-subscription pass (capability, scheduled
// conflicts, #781 notice window) without writing anything.
func (s *PlanMigrationService) classify(ctx context.Context, req *PlanMigrationRequest, source, target *models.Price) (cohort []*models.Subscription, outcomes []PlanMigrationOutcome, byRail map[string]*RailCounts, err error) {
	cohort, err = s.reprice.repo.ListMigratableSubscriptionsByPriceID(ctx, source.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("plan migration: list cohort: %w", err)
	}
	byRail = map[string]*RailCounts{}
	rail := func(sub *models.Subscription) *RailCounts {
		key := string(sub.Rail)
		if byRail[key] == nil {
			byRail[key] = &RailCounts{}
		}
		return byRail[key]
	}
	for _, sub := range cohort {
		out := PlanMigrationOutcome{SubscriptionID: sub.ID, Rail: string(sub.Rail)}
		switch {
		case sub.PriceID == target.ID:
			out.Disposition = "skipped"
			out.Reason = "already on target price"
			rail(sub).Skipped++
		case s.classifyMigrationCapability(ctx, sub) == capabilityUserAction:
			out.Disposition = "blocked"
			out.Reason = migrationBlockedRailUserAction
			rail(sub).RequiresAction++
		default:
			if err := s.reprice.scheduledConflict(ctx, sub.ID); err != nil {
				out.Disposition = "skipped"
				out.Reason = err.Error()
				rail(sub).Skipped++
				break
			}
			violates, nerr := s.reprice.checkNoticeWindow(ctx, source, target, req.EffectiveAt)
			if nerr != nil {
				return nil, nil, nil, fmt.Errorf("plan migration: notice window: %w", nerr)
			}
			if violates && !req.AcknowledgeShortNotice {
				out.Disposition = "skipped"
				out.Reason = (&RepriceConstraintError{Sentinel: ErrRepriceNoticeWindowViolation, SubscriptionID: sub.ID, FromPriceID: source.ID, ToPriceID: target.ID}).Error()
				rail(sub).Skipped++
				break
			}
			out.Disposition = "scheduled"
			rail(sub).Auto++
		}
		outcomes = append(outcomes, out)
	}
	return cohort, outcomes, byRail, nil
}

// Preview classifies the whole cohort WITHOUT writing anything — the
// operator's commit gate: per-rail auto/requires-action/skip counts and the
// full per-subscription ledger.
func (s *PlanMigrationService) Preview(ctx context.Context, req PlanMigrationRequest) (*PlanMigrationResult, error) {
	fallback, err := normalizeFallback(req.FallbackPolicy)
	if err != nil {
		return nil, err
	}
	source, target, err := s.resolveMigration(ctx, &req)
	if err != nil {
		return nil, err
	}
	cohort, outcomes, byRail, err := s.classify(ctx, &req, source, target)
	if err != nil {
		return nil, err
	}
	res := &PlanMigrationResult{
		SourcePriceID:  source.ID,
		TargetPriceID:  target.ID,
		EffectiveAt:    req.EffectiveAt,
		FallbackPolicy: fallback,
		Matched:        len(cohort),
		ByRail:         byRail,
		Outcomes:       outcomes,
	}
	for _, o := range outcomes {
		switch o.Disposition {
		case "scheduled":
			res.Scheduled++
		case "skipped":
			res.Skipped++
		case "blocked":
			res.Blocked++
		}
	}
	return res, nil
}

// Migrate executes the plan migration: batch header + per-subscription rows,
// source archive, rail pushes, notifications. Partial rail-push failures
// degrade the affected rows to blocked (with the push error) instead of
// failing the whole batch — the ledger stays complete and a re-run migrates
// the remainder (already-scheduled subs skip via the one-scheduled conflict).
func (s *PlanMigrationService) Migrate(ctx context.Context, req PlanMigrationRequest) (*PlanMigrationResult, error) {
	fallback, err := normalizeFallback(req.FallbackPolicy)
	if err != nil {
		return nil, err
	}
	source, target, err := s.resolveMigration(ctx, &req)
	if err != nil {
		return nil, err
	}
	targetProduct, err := s.reprice.products(ctx, target.ProductID)
	if err != nil {
		return nil, fmt.Errorf("plan migration: target product: %w", err)
	}
	cohort, outcomes, byRail, err := s.classify(ctx, &req, source, target)
	if err != nil {
		return nil, err
	}

	res := &PlanMigrationResult{
		SourcePriceID:  source.ID,
		TargetPriceID:  target.ID,
		EffectiveAt:    req.EffectiveAt,
		FallbackPolicy: fallback,
		Matched:        len(cohort),
		ByRail:         byRail,
	}
	subByID := make(map[uuid.UUID]*models.Subscription, len(cohort))
	for _, sub := range cohort {
		subByID[sub.ID] = sub
	}
	for _, o := range outcomes {
		switch o.Disposition {
		case "scheduled":
			res.Scheduled++
		case "skipped":
			res.Skipped++
		case "blocked":
			res.Blocked++
		}
	}

	batch, err := s.reprice.repo.CreatePlanMigrationBatch(ctx, source.ID, target.ID, req.EffectiveAt, fallback, res.Matched, res.Scheduled, res.Skipped, res.Blocked)
	if err != nil {
		return nil, fmt.Errorf("plan migration: create batch: %w", err)
	}
	res.BatchID = &batch.ID
	batchID := batch.ID

	for i := range outcomes {
		o := &outcomes[i]
		sub := subByID[o.SubscriptionID]
		switch o.Disposition {
		case "blocked":
			row, berr := s.reprice.repo.CreateBlockedReprice(ctx, sub.ID, sub.PriceID, target.ID, req.EffectiveAt, &batchID, models.RepriceKindPlanChange, o.Reason)
			if berr != nil {
				return nil, fmt.Errorf("plan migration: record blocked subscription %s: %w", sub.ID, berr)
			}
			o.RepriceID = &row.ID
		case "scheduled":
			row, cerr := s.reprice.repo.CreatePlanChangeReprice(ctx, sub.ID, sub.PriceID, target.ID, req.EffectiveAt, &batchID, req.AcknowledgeShortNotice)
			if cerr != nil {
				return nil, fmt.Errorf("plan migration: schedule subscription %s: %w", sub.ID, cerr)
			}
			o.RepriceID = &row.ID
			if perr := s.executeScheduled(ctx, &req, sub, source, target, targetProduct, row); perr != nil {
				// Degrade this row to blocked; keep the batch going.
				if berr := s.reprice.repo.BlockScheduledReprice(ctx, row.ID, migrationBlockedRailPushFailed+": "+perr.Error()); berr != nil {
					return nil, fmt.Errorf("plan migration: block failed push for %s: %w", sub.ID, berr)
				}
				o.Disposition = "blocked"
				o.Reason = migrationBlockedRailPushFailed + ": " + perr.Error()
				res.Scheduled--
				res.Blocked++
				if rc := byRail[string(sub.Rail)]; rc != nil {
					rc.Auto--
					rc.RequiresAction++
				}
				continue
			}
			if req.Immediate && s.classifyMigrationCapability(ctx, sub) == capabilityAutoInternal {
				o.Disposition = "applied_immediately"
			}
			s.reprice.emitPlanChangeNotification(ctx, sub, source, target, targetProduct, req.EffectiveAt)
		}
	}

	// Archive the source price: the retired plan stops selling the moment the
	// migration is committed (grandfathering of anything left behind — the
	// blocked rows — is the archived-price billing path that already exists).
	if req.ArchiveSource == nil || *req.ArchiveSource {
		if err := s.reprice.prices.SetArchived(ctx, source.ID, true); err != nil {
			return nil, fmt.Errorf("plan migration: archive source price: %w", err)
		}
		res.SourceArchived = true
	}

	res.Outcomes = outcomes
	log.WithContext(ctx).WithFields(log.Fields{
		"batch_id":     batch.ID,
		"source_price": source.ID,
		"target_price": target.ID,
		"effective_at": req.EffectiveAt,
		"matched":      res.Matched,
		"scheduled":    res.Scheduled,
		"skipped":      res.Skipped,
		"blocked":      res.Blocked,
	}).Info("plan migration committed")
	return res, nil
}

// executeScheduled performs the per-rail action for one scheduled row.
func (s *PlanMigrationService) executeScheduled(ctx context.Context, req *PlanMigrationRequest, sub *models.Subscription, source, target *models.Price, targetProduct *models.Product, row *models.SubscriptionReprice) error {
	switch s.classifyMigrationCapability(ctx, sub) {
	case capabilityAutoStripe:
		return s.pushStripe(ctx, req, sub, source, target, row)
	case capabilityAutoInternal:
		if req.Immediate {
			return s.applyImmediately(ctx, sub, target, targetProduct, row)
		}
		return nil // the renewal-boundary pickup is the whole mechanism
	default:
		return fmt.Errorf("unexpected capability for scheduled row")
	}
}

// pushStripe pushes the plan change to Stripe. Boundary mode schedules the
// flip at the current period end (Stripe's own phase change — no proration,
// no anchor move); Immediate re-points the item now with
// proration_behavior=none (nothing charged until the next invoice). The
// INTERNAL cutover is applied by converge from fetched provider truth, which
// also marks the row applied (ApplyScheduledRepriceForSubscriptionPrice).
func (s *PlanMigrationService) pushStripe(ctx context.Context, req *PlanMigrationRequest, sub *models.Subscription, source, target *models.Price, row *models.SubscriptionReprice) error {
	if s.stripe == nil {
		return fmt.Errorf("stripe pusher not configured")
	}
	targetStripeID, ok := target.GetStripeConfig()
	if !ok || strings.TrimSpace(targetStripeID) == "" {
		return fmt.Errorf("%s", migrationBlockedMissingRailPrice)
	}
	railSubID := strings.TrimSpace(sub.RailSubscriptionID)
	if railSubID == "" {
		return fmt.Errorf("subscription missing stripe reference")
	}
	if req.Immediate {
		itemID, err := s.stripe.GetSubscriptionItemID(ctx, railSubID)
		if err != nil {
			return err
		}
		return s.stripe.UpdateSubscriptionPrice(ctx, railSubID, itemID, targetStripeID, target.ID.String(), "none", "")
	}
	sourceStripeID, ok := source.GetStripeConfig()
	if !ok || strings.TrimSpace(sourceStripeID) == "" {
		return fmt.Errorf("%s", migrationBlockedMissingRailPrice)
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("subscription missing current period end")
	}
	// A Stripe subscription schedule flips at the CURRENT period end. If
	// effective_at lies beyond it, pushing now would flip a whole period
	// EARLY — a money defect. Refuse honestly; the row lands blocked and an
	// idempotent re-run inside the final pre-effective period succeeds.
	// (Automated deferred push is the filed follow-up on #813.)
	if req.EffectiveAt.After(*sub.CurrentPeriodEndsAt) {
		return fmt.Errorf("stripe_deferred_push_required: effective_at %s is beyond the current period end %s — re-run the migration once the subscription enters its final period before the effective date", req.EffectiveAt.UTC().Format(time.RFC3339), sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339))
	}
	periodStart := sub.StartedAt
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		periodStart = *sub.CurrentPeriodStartsAt
	}
	_, err := s.stripe.ScheduleSubscriptionPriceChange(ctx, railSubID, sourceStripeID, targetStripeID, periodStart, *sub.CurrentPeriodEndsAt, target.RecurringCycleDays())
	return err
}

// applyImmediately (#813 Immediate, engine-driven rails): cut the product /
// entitlement snapshots and price over NOW — entitlement windows re-derive at
// the next renewal grant; nothing is charged here — and mark the row applied.
func (s *PlanMigrationService) applyImmediately(ctx context.Context, sub *models.Subscription, target *models.Price, targetProduct *models.Product, row *models.SubscriptionReprice) error {
	sub.PriceID = target.ID
	sub.ProductID = target.ProductID
	sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(targetProduct.EntitlementsSpec)
	sub.CreditsSpecSnapshot = models.CloneCreditsSpec(targetProduct.CreditsSpec)
	if err := s.reprice.subscriptions.Update(ctx, sub); err != nil {
		return fmt.Errorf("apply immediately: %w", err)
	}
	if err := s.reprice.repo.Apply(ctx, row.ID); err != nil && !errors.Is(err, ErrRepriceNotScheduled) {
		return fmt.Errorf("apply immediately: mark applied: %w", err)
	}
	return nil
}

// GetBatch returns a migration batch header plus its per-subscription rows.
func (s *PlanMigrationService) GetBatch(ctx context.Context, batchID uuid.UUID, limit, offset int) (*models.RepriceBatch, []*models.SubscriptionReprice, error) {
	batch, err := s.reprice.repo.GetBatchByID(ctx, batchID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.reprice.repo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: &batchID}, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	return batch, rows, nil
}

// CancelBatch cancels every still-scheduled row in the batch (rows already
// applied or blocked are untouched). It does NOT un-archive the source price.
// NOTE (honest limitation): a Stripe boundary push that already created a
// subscription schedule is not rolled back here — converge keeps provider
// truth authoritative; cancel before pushes land, or release the Stripe
// schedule out of band.
func (s *PlanMigrationService) CancelBatch(ctx context.Context, batchID uuid.UUID) (int, error) {
	rows, err := s.reprice.repo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: &batchID}, 10000, 0)
	if err != nil {
		return 0, err
	}
	canceled := 0
	for _, row := range rows {
		if row.Status != models.RepriceStatusScheduled {
			continue
		}
		if err := s.reprice.Cancel(ctx, row.ID); err != nil {
			if errors.Is(err, ErrRepriceNotScheduled) || errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return canceled, err
		}
		canceled++
	}
	return canceled, nil
}

// emitPlanChangeNotification fires the #813 plan-change disclosure at
// SCHEDULE time — distinct event from #773's reprice_scheduled because the
// legal content differs: the PLAN is changing, not just the amount.
func (s *RepriceService) emitPlanChangeNotification(ctx context.Context, sub *models.Subscription, from, to *models.Price, toProduct *models.Product, effectiveAt time.Time) {
	if s.notifications == nil {
		return
	}
	n := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: sub.CustomerID,
		EventType:  models.NotificationSubscriptionPlanChangeScheduled,
		Data: map[string]any{
			"subscription_id": sub.ID.String(),
			"from_price_id":   from.ID.String(),
			"to_price_id":     to.ID.String(),
			"to_product_id":   toProduct.ID.String(),
			"to_product_name": toProduct.DisplayName,
			"old_amount":      from.Amount,
			"new_amount":      to.Amount,
			"currency":        to.Currency,
			"effective_at":    effectiveAt.UTC().Format(time.RFC3339),
		},
	}
	if err := s.notifications.CreateAndDeliver(ctx, n); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("failed to emit subscription_plan_change_scheduled notification")
	}
}
