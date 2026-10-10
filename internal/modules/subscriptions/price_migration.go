package subscriptions

// Price migrations move the subscribers of one price, or of every version of
// a price key, to another price.
//
// Money: nothing is prorated, charged early or re-anchored. Each subscription
// moves at its first renewal on or after effective_at, carrying the move as
// its scheduled change until then. Per rail:
//   - engine-owned (OpenRails charges): the renewal applies the change.
//   - Stripe, provider-owned: the change is pushed as a Stripe subscription
//     schedule flipping at period end; converge applies it from Stripe's truth.
//   - an NMI gateway schedule: the amount is pushed (update_subscription) once
//     the subscription is in its last period before effective_at, and the
//     subscription moves at once, since NMI bills the new amount from its next
//     rebill; a push before then would flip a period early, so the move
//     blocks and the re-driver pushes it in time.
//   - CCBill, Solana, an NMI schedule no declared account reaches: blocked
//     (rail_requires_user_action); the fallback policy records intent.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	// ErrRebillTermsCommitted prevents a price change from revoking an accepted
	// renewal whose provider preparation or money submission may have occurred.
	ErrRebillTermsCommitted = apperr.New(http.StatusConflict, "rebill_terms_committed", "accepted recurring payment owns the pending price terms")
	// ErrMembershipSlotTaken: the customer already holds a subscription of the
	// product or its tier group (or one whose provider stop is pending).
	ErrMembershipSlotTaken = errors.New("membership slot taken")
	// ErrRenewalInProgress refuses a cancel while an accepted renewal payment is
	// unresolved; the cancel succeeds once the payment resolves.
	ErrRenewalInProgress = apperr.New(http.StatusConflict, "payment_in_progress", "a renewal payment for this subscription is unresolved; cancel again once it resolves")

	ErrPriceCurrencyMismatch       = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceChangeCurrencyMismatch, "the target price must be in the subscription's currency")
	ErrPriceTargetArchived         = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceChangeTargetArchived, "the target price is archived")
	ErrPriceIncreaseNoticeTooShort = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceIncreaseNoticeTooShort, "effective_at is inside the merchant's notice window for a price increase")
	ErrPriceMigrationNotFound      = apperr.New(http.StatusNotFound, billing.CodePriceMigrationNotFound, "price migration not found")

	errMigrationPriceNotFound   = apperr.New(http.StatusNotFound, "price_not_found", "price not found")
	errMigrationProductNotFound = apperr.New(http.StatusNotFound, "product_not_found", "product not found")
	errMigrationKeyNotFound     = apperr.New(http.StatusNotFound, "price_key_not_found", "no price holds this key")
)

// DefaultPriceIncreaseNoticeDays is the notice window for a price increase
// when the merchant configures none.
const DefaultPriceIncreaseNoticeDays = 30

// Blocked reasons, stable: they are the reason of a blocked move.
const (
	migrationBlockedRailUserAction   = "rail_requires_user_action"
	migrationBlockedMissingRailPrice = "target_price_missing_rail_config"
	migrationBlockedRailPushFailed   = "rail_push_failed"
	// An NMI amount update cannot change the billing interval.
	migrationBlockedNMIIntervalMismatch = "nmi_interval_mismatch"
	// NMI bills whole minor units.
	migrationBlockedNMISubCentAmount = "nmi_amount_not_cent_representable"
)

// StripePusher pushes a migration to Stripe (*StripeService; faked in tests).
type StripePusher interface {
	GetSubscriptionItemID(ctx context.Context, subscriptionID string) (string, error)
	UpdateSubscriptionPrice(ctx context.Context, subscriptionID, itemID, newPriceID, internalPriceID, prorationBehavior, billingAnchor string) error
	ScheduleSubscriptionPriceChange(ctx context.Context, subscriptionID, currentPriceID, newPriceID string, currentPeriodStart, currentPeriodEnd time.Time, billingCycleDays *int) (string, error)
}

// PriceMigrationService creates, previews, reads and cancels price
// migrations, and re-drives their failed pushes.
type PriceMigrationService struct {
	db            *db.DB
	prices        *catalog.PriceService
	subscriptions *SubscriptionService
	notifications *NotificationService
	config        *merchantconfig.Store
	stripe        StripePusher
	nmi           NMIPusher
	clock         clockwork.Clock
}

func NewPriceMigrationService(d *db.DB, prices *catalog.PriceService, subscriptions *SubscriptionService, notifications *NotificationService, config *merchantconfig.Store, stripe StripePusher, nmi NMIPusher, clock clockwork.Clock) *PriceMigrationService {
	return &PriceMigrationService{db: d, prices: prices, subscriptions: subscriptions, notifications: notifications, config: config, stripe: stripe, nmi: nmi, clock: timeutil.FirstClock(clock)}
}

func (s *PriceMigrationService) now() time.Time { return s.clock.Now() }

// migrationPlan is a resolved request.
type migrationPlan struct {
	fromPrice   *models.Price
	product     *models.Product
	key         string
	sources     map[uuid.UUID]*models.Price
	target      *models.Price // nil only in a preview by key
	effectiveAt time.Time
	fallback    billing.MigrationFallback
	acknowledge bool
	archive     bool
}

func (s *PriceMigrationService) resolve(ctx context.Context, p billing.CreatePriceMigrationParams, preview bool) (*migrationPlan, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	byKey := strings.TrimSpace(p.ProductKey) != "" || strings.TrimSpace(p.PriceKey) != ""
	if byKey == !p.FromPriceID.IsZero() || (byKey && (strings.TrimSpace(p.ProductKey) == "" || strings.TrimSpace(p.PriceKey) == "")) {
		return nil, apperr.Invalidf("name from_price_id, or product_key and price_key")
	}
	plan := &migrationPlan{sources: map[uuid.UUID]*models.Price{}, effectiveAt: p.EffectiveAt, acknowledge: p.AcknowledgeShortNotice, archive: p.ArchiveSource == nil || *p.ArchiveSource}
	switch p.FallbackPolicy {
	case "", billing.MigrationKeepGrandfathered:
		plan.fallback = billing.MigrationKeepGrandfathered
	case billing.MigrationCancelAtPeriodEnd:
		plan.fallback = billing.MigrationCancelAtPeriodEnd
	default:
		return nil, apperr.Invalidf("fallback_policy must be keep_grandfathered or cancel_at_period_end")
	}
	if plan.effectiveAt.IsZero() {
		plan.effectiveAt = s.now()
	}
	plan.effectiveAt = plan.effectiveAt.UTC()
	if p.ToPriceID.IsZero() {
		if !preview || !byKey {
			return nil, apperr.Invalidf("to_price_id is required")
		}
	} else {
		if plan.target, err = s.price(ctx, p.ToPriceID.UUID()); err != nil {
			return nil, err
		}
		if plan.target.Archived {
			return nil, ErrPriceTargetArchived
		}
	}
	if !byKey {
		if plan.fromPrice, err = s.price(ctx, p.FromPriceID.UUID()); err != nil {
			return nil, err
		}
		if plan.target != nil && plan.fromPrice.ID == plan.target.ID {
			return nil, apperr.Invalidf("from_price_id and to_price_id name the same price")
		}
		if plan.target != nil && !sameCurrency(plan.fromPrice, plan.target) {
			return nil, ErrPriceCurrencyMismatch
		}
		plan.sources[plan.fromPrice.ID] = plan.fromPrice
		return plan, nil
	}
	plan.key = strings.TrimSpace(p.PriceKey)
	plan.product, err = catalog.NewProductService(s.db).GetByKey(ctx, strings.TrimSpace(p.ProductKey))
	if db.IsNotFound(err) {
		return nil, errMigrationProductNotFound
	}
	if err != nil {
		return nil, err
	}
	chain, err := s.prices.ListChainByKey(ctx, mid.UUID(), plan.product.ID, plan.key)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, errMigrationKeyNotFound
	}
	for _, price := range chain {
		if plan.target == nil || price.ID != plan.target.ID {
			plan.sources[price.ID] = price
		}
	}
	return plan, nil
}

func (s *PriceMigrationService) price(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	price, err := s.prices.GetByID(ctx, id)
	if db.IsNotFound(err) {
		return nil, errMigrationPriceNotFound
	}
	return price, err
}

func sameCurrency(a, b *models.Price) bool {
	return strings.EqualFold(strings.TrimSpace(a.Currency), strings.TrimSpace(b.Currency))
}

func (s *PriceMigrationService) cohort(ctx context.Context, plan *migrationPlan) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(plan.sources))
	for id := range plan.sources {
		ids = append(ids, id)
	}
	rows, err := s.db.Gen(ctx).ListMigratableSubscriptionsByPriceIDs(ctx, gen.ListMigratableSubscriptionsByPriceIDsParams{MerchantID: mid.UUID(), PriceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("price migration: list subscribers: %w", err)
	}
	out := make([]*models.Subscription, 0, len(rows))
	for _, row := range rows {
		sub, err := models.SubscriptionFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

// migrationCapability is what a subscription's rail lets a migration do.
type migrationCapability int

const (
	capabilityEngine     migrationCapability = iota // the renewal applies it
	capabilityAutoStripe                            // push a Stripe schedule
	capabilityAutoNMI                               // push the NMI amount
	capabilityUserAction                            // the provider cannot be moved from here
)

func (s *PriceMigrationService) capability(ctx context.Context, sub *models.Subscription) migrationCapability {
	switch {
	case sub.Rail == models.RailCCBill, sub.Rail == models.RailSolana:
		// A redirect flow or the customer's on-chain signature.
		return capabilityUserAction
	case sub.CollectionPolicy == models.CollectionPolicyEngine:
		return capabilityEngine
	case sub.Rail == models.RailStripe:
		return capabilityAutoStripe
	case s.nmi != nil && s.nmi.CanPush(ctx, sub):
		// A gateway schedule keeps charging its own amount, so it is pushed
		// even when a stored credential exists.
		return capabilityAutoNMI
	default:
		return capabilityUserAction
	}
}

// nmiCyclesCompatible: an NMI amount update never touches the schedule.
func nmiCyclesCompatible(source, target *models.Price) bool {
	src, tgt := source.RecurringCycleDays(), target.RecurringCycleDays()
	return src != nil && tgt != nil && *src == *tgt
}

// railMinorRepresentable: the amount lands on a whole NMI minor unit.
func railMinorRepresentable(currency string, amountNative int64) bool {
	_, err := moneyutil.NativeToRailMinorExact(currency, amountNative)
	return err == nil
}

// noticeViolated: a price increase whose effective_at is inside the
// merchant's notice window. Decreases never are.
func (s *PriceMigrationService) noticeViolated(ctx context.Context, from, to *models.Price, effectiveAt time.Time) (bool, error) {
	if to.Amount <= from.Amount {
		return false, nil
	}
	days := DefaultPriceIncreaseNoticeDays
	if s.config != nil {
		cfg, _, err := s.config.Get(ctx)
		if err != nil {
			return false, fmt.Errorf("price migration: notice window: %w", err)
		}
		if cfg.RepriceNoticeWindowDays != nil {
			days = *cfg.RepriceNoticeWindowDays
		}
	}
	return days > 0 && effectiveAt.Before(s.now().Add(time.Duration(days)*24*time.Hour)), nil
}

type classified struct {
	sub          *models.Subscription
	from         *models.Price
	capability   migrationCapability
	acknowledged bool
	outcome      billing.PriceMigrationOutcome
}

// classify decides each subscription's disposition without writing.
func (s *PriceMigrationService) classify(ctx context.Context, plan *migrationPlan, cohort []*models.Subscription) ([]*classified, map[string]*billing.PriceMigrationRailCounts, error) {
	ids := make([]uuid.UUID, 0, len(cohort))
	for _, sub := range cohort {
		ids = append(ids, sub.ID)
	}
	pending, err := PendingChanges(ctx, s.db, ids)
	if err != nil {
		return nil, nil, err
	}
	byRail := map[string]*billing.PriceMigrationRailCounts{}
	out := make([]*classified, 0, len(cohort))
	for _, sub := range cohort {
		c := &classified{sub: sub, from: plan.sources[sub.PriceID], capability: s.capability(ctx, sub)}
		c.outcome = billing.PriceMigrationOutcome{SubscriptionID: billing.SubscriptionID(sub.ID), Rail: string(sub.Rail)}
		rail := byRail[string(sub.Rail)]
		if rail == nil {
			rail = &billing.PriceMigrationRailCounts{}
			byRail[string(sub.Rail)] = rail
		}
		skip := func(reason string) {
			c.outcome.Disposition, c.outcome.Reason = billing.MigrationSkipped, &reason
			rail.Skipped++
		}
		block := func(reason string) {
			c.outcome.Disposition, c.outcome.Reason = billing.MigrationBlocked, &reason
			rail.RequiresAction++
		}
		switch {
		case pending[sub.ID] != nil:
			skip(ErrChangeAlreadyScheduled.Error())
		case !sameCurrency(c.from, plan.target):
			skip(ErrPriceCurrencyMismatch.Error())
		case c.capability == capabilityUserAction:
			block(migrationBlockedRailUserAction)
		case c.capability == capabilityAutoNMI && !nmiCyclesCompatible(c.from, plan.target):
			block(migrationBlockedNMIIntervalMismatch)
		case c.capability == capabilityAutoNMI && !railMinorRepresentable(plan.target.Currency, plan.target.Amount):
			block(migrationBlockedNMISubCentAmount)
		default:
			violated, err := s.noticeViolated(ctx, c.from, plan.target, plan.effectiveAt)
			if err != nil {
				return nil, nil, err
			}
			if violated && !plan.acknowledge {
				skip(ErrPriceIncreaseNoticeTooShort.Error())
				break
			}
			c.acknowledged = violated
			c.outcome.Disposition = billing.MigrationScheduled
			rail.Auto++
		}
		out = append(out, c)
	}
	return out, byRail, nil
}

// Preview is what Create would do, with nothing written.
func (s *PriceMigrationService) Preview(ctx context.Context, p billing.CreatePriceMigrationParams) (*billing.PriceMigrationPreview, error) {
	plan, err := s.resolve(ctx, p, true)
	if err != nil {
		return nil, err
	}
	cohort, err := s.cohort(ctx, plan)
	if err != nil {
		return nil, err
	}
	out := &billing.PriceMigrationPreview{EffectiveAt: plan.effectiveAt, Matched: len(cohort), ByRail: map[string]*billing.PriceMigrationRailCounts{}, Outcomes: []billing.PriceMigrationOutcome{}}
	if plan.target == nil {
		return out, nil
	}
	target := billing.PriceID(plan.target.ID)
	out.ToPriceID = &target
	classes, byRail, err := s.classify(ctx, plan, cohort)
	if err != nil {
		return nil, err
	}
	out.ByRail = byRail
	for _, c := range classes {
		out.Outcomes = append(out.Outcomes, c.outcome)
		switch c.outcome.Disposition {
		case billing.MigrationScheduled:
			out.Scheduled++
		case billing.MigrationSkipped:
			out.Skipped++
		case billing.MigrationBlocked:
			out.Blocked++
		}
	}
	return out, nil
}

// Create records the migration and moves each classified subscription. A
// failed provider push blocks that subscription's move and the rest go on;
// the re-driver retries it.
func (s *PriceMigrationService) Create(ctx context.Context, p billing.CreatePriceMigrationParams) (*billing.PriceMigration, error) {
	plan, err := s.resolve(ctx, p, false)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	targetProduct, err := catalog.NewProductService(s.db).GetByID(ctx, plan.target.ProductID)
	if err != nil {
		return nil, fmt.Errorf("price migration: target product: %w", err)
	}
	cohort, err := s.cohort(ctx, plan)
	if err != nil {
		return nil, err
	}
	classes, _, err := s.classify(ctx, plan, cohort)
	if err != nil {
		return nil, err
	}
	params := gen.CreatePriceMigrationParams{MerchantID: mid.UUID(), ToPriceID: plan.target.ID, EffectiveAt: plan.effectiveAt, FallbackPolicy: string(plan.fallback), SubscriptionsMatched: int32(len(cohort))} // #nosec G115 -- a page of subscribers
	if plan.fromPrice != nil {
		params.FromPriceID = &plan.fromPrice.ID
	} else {
		params.FromProductID, params.FromPriceKey = &plan.product.ID, &plan.key
	}
	row, err := s.db.Gen(ctx).CreatePriceMigration(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("price migration: create: %w", err)
	}
	migration := row.ID
	skipped := 0
	for _, c := range classes {
		switch c.outcome.Disposition {
		case billing.MigrationSkipped:
			skipped++
		case billing.MigrationBlocked:
			if _, err := s.db.Gen(ctx).CreateBlockedScheduledChange(ctx, gen.CreateBlockedScheduledChangeParams{
				MerchantID: mid.UUID(), SubscriptionID: c.sub.ID, FromPriceID: c.sub.PriceID, PriceID: plan.target.ID,
				EffectiveAt: plan.effectiveAt, PriceMigrationID: migration, BlockedReason: *c.outcome.Reason,
			}); err != nil {
				return nil, fmt.Errorf("price migration: record blocked %s: %w", c.sub.ID, err)
			}
		case billing.MigrationScheduled:
			moved, err := s.move(ctx, c, plan.target, targetProduct, plan.effectiveAt, migration)
			if err != nil {
				return nil, err
			}
			if !moved {
				skipped++
			}
		}
	}
	if _, err := s.db.Gen(ctx).SetPriceMigrationSkipped(ctx, gen.SetPriceMigrationSkippedParams{MerchantID: mid.UUID(), ID: migration, SubscriptionsSkipped: int32(skipped)}); err != nil { // #nosec G115 -- at most matched
		return nil, err
	}
	if plan.archive {
		for _, source := range plan.sources {
			if source.Archived {
				continue
			}
			if err := s.prices.SetArchived(ctx, source.ID, true); err != nil {
				return nil, fmt.Errorf("price migration: archive %s: %w", source.ID, err)
			}
		}
	}
	log.WithContext(ctx).WithFields(log.Fields{"price_migration_id": migration, "to_price_id": plan.target.ID, "effective_at": plan.effectiveAt, "matched": len(cohort), "skipped": skipped}).Info("price migration created")
	return s.Get(ctx, billing.PriceMigrationID(migration))
}

// move schedules one subscription's change and pushes it to its provider.
// It answers false when the subscription changed since classification.
func (s *PriceMigrationService) move(ctx context.Context, c *classified, target *models.Price, targetProduct *models.Product, effectiveAt time.Time, migration uuid.UUID) (bool, error) {
	var change *models.ScheduledChange
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, c.sub.ID)
		if err != nil {
			return err
		}
		if sub.PriceID != c.from.ID {
			return errChangeNotScheduled
		}
		change, err = ScheduleChange(ctx, d, sub, NewScheduledChange{PriceID: target.ID, EffectiveAt: effectiveAt, Source: billing.ScheduledChangeMigration, PriceMigrationID: &migration, AcknowledgedShortNotice: c.acknowledged}, s.now())
		c.sub = sub
		return err
	})
	if errors.Is(err, errChangeNotScheduled) || errors.Is(err, ErrChangeAlreadyScheduled) || errors.Is(err, ErrRebillTermsCommitted) {
		reason := err.Error()
		c.outcome.Disposition, c.outcome.Reason = billing.MigrationSkipped, &reason
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("price migration: schedule %s: %w", c.sub.ID, err)
	}
	if err := s.push(ctx, c.capability, c.sub, c.from, target, targetProduct, change); err != nil {
		reason := migrationBlockedRailPushFailed + ": " + err.Error()
		if berr := s.block(ctx, change.ID, reason); berr != nil {
			return false, fmt.Errorf("price migration: block failed push for %s: %w", c.sub.ID, berr)
		}
		c.outcome.Disposition, c.outcome.Reason = billing.MigrationBlocked, &reason
		return true, nil
	}
	if c.capability == capabilityAutoNMI {
		c.outcome.Disposition = billing.MigrationApplied
	}
	s.notify(ctx, c.sub, c.from, target, targetProduct, effectiveAt)
	return true, nil
}

func (s *PriceMigrationService) block(ctx context.Context, id uuid.UUID, reason string) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.Gen(ctx).BlockScheduledChange(ctx, gen.BlockScheduledChangeParams{MerchantID: mid.UUID(), ID: id, BlockedReason: reason})
	return err
}

// push performs the rail's part of one scheduled move.
func (s *PriceMigrationService) push(ctx context.Context, capability migrationCapability, sub *models.Subscription, source, target *models.Price, targetProduct *models.Product, change *models.ScheduledChange) error {
	switch capability {
	case capabilityEngine:
		return nil
	case capabilityAutoStripe:
		return s.pushStripe(ctx, sub, source, target, change.EffectiveAt)
	case capabilityAutoNMI:
		return s.pushNMI(ctx, sub, target, targetProduct, change)
	default:
		return fmt.Errorf("unexpected capability for a scheduled move")
	}
}

// deferredPushRequired: an effective date past the current period means a
// push now would flip at least one rebill early.
func deferredPushRequired(effectiveAt time.Time, sub *models.Subscription) bool {
	return sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero() && effectiveAt.After(*sub.CurrentPeriodEndsAt)
}

// pushNMI points the gateway schedule's amount at the target (schedule and
// plan_payments untouched, read back) and moves the subscription now: NMI
// bills the new amount from its next rebill. Nothing is charged here.
func (s *PriceMigrationService) pushNMI(ctx context.Context, sub *models.Subscription, target *models.Price, targetProduct *models.Product, change *models.ScheduledChange) error {
	if s.nmi == nil {
		return fmt.Errorf("nmi pusher not configured")
	}
	if strings.TrimSpace(sub.RailSubscriptionID) == "" {
		return fmt.Errorf("subscription missing nmi reference")
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("subscription missing current period end")
	}
	if deferredPushRequired(change.EffectiveAt, sub) {
		return fmt.Errorf("nmi_deferred_push_required: effective_at %s is beyond the current period end %s", change.EffectiveAt.UTC().Format(time.RFC3339), sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339))
	}
	if err := s.nmi.PushPlanAmount(ctx, sub, target.Currency, target.Amount); err != nil {
		return err
	}
	return s.applyNow(ctx, sub.ID, change, target, targetProduct)
}

// pushStripe plants a Stripe subscription schedule flipping at the current
// period end (no proration, no anchor move); converge applies the change
// from Stripe's truth.
func (s *PriceMigrationService) pushStripe(ctx context.Context, sub *models.Subscription, source, target *models.Price, effectiveAt time.Time) error {
	if s.stripe == nil {
		return fmt.Errorf("stripe pusher not configured")
	}
	targetStripeID, ok := target.GetStripeConfig()
	if !ok || strings.TrimSpace(targetStripeID) == "" {
		return fmt.Errorf("%s", migrationBlockedMissingRailPrice)
	}
	sourceStripeID, ok := source.GetStripeConfig()
	if !ok || strings.TrimSpace(sourceStripeID) == "" {
		return fmt.Errorf("%s", migrationBlockedMissingRailPrice)
	}
	railSubID := strings.TrimSpace(sub.RailSubscriptionID)
	if railSubID == "" {
		return fmt.Errorf("subscription missing stripe reference")
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("subscription missing current period end")
	}
	if deferredPushRequired(effectiveAt, sub) {
		return fmt.Errorf("stripe_deferred_push_required: effective_at %s is beyond the current period end %s", effectiveAt.UTC().Format(time.RFC3339), sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339))
	}
	periodStart := sub.StartedAt
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		periodStart = *sub.CurrentPeriodStartsAt
	}
	_, err := s.stripe.ScheduleSubscriptionPriceChange(ctx, railSubID, sourceStripeID, targetStripeID, periodStart, *sub.CurrentPeriodEndsAt, target.RecurringCycleDays())
	return err
}

// applyNow moves the subscription to the target now and marks the change
// applied; its access re-derives at the next renewal grant.
func (s *PriceMigrationService) applyNow(ctx context.Context, subscriptionID uuid.UUID, change *models.ScheduledChange, target *models.Price, targetProduct *models.Product) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		repo := NewSubscriptionRepo(d)
		sub, err := repo.GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if sub.PriceID != change.FromPriceID {
			return fmt.Errorf("apply now: the subscription's price changed")
		}
		sub.PriceID, sub.ProductID = target.ID, targetProduct.ID
		if err := repo.UpdateAt(ctx, sub, s.now()); err != nil {
			return fmt.Errorf("apply now: %w", err)
		}
		return ApplyChange(ctx, d, change.ID, s.now())
	})
}

// notify sends the schedule-time notice: a plan change when the product
// changes (the legal content differs), else a price change.
func (s *PriceMigrationService) notify(ctx context.Context, sub *models.Subscription, from, to *models.Price, toProduct *models.Product, effectiveAt time.Time) {
	if s.notifications == nil {
		return
	}
	effective := effectiveAt.UTC()
	data := billing.NotificationData{
		SubscriptionID: billing.SubscriptionID(sub.ID), FromPriceID: billing.PriceID(from.ID), ToPriceID: billing.PriceID(to.ID),
		OldAmount: &from.Amount, NewAmount: &to.Amount, Currency: to.Currency, EffectiveAt: &effective,
	}
	event := models.NotificationSubscriptionRepriceScheduled
	if to.ProductID != from.ProductID {
		event = models.NotificationSubscriptionPlanChangeScheduled
		data.ToProductID, data.ToProductName = billing.ProductID(toProduct.ID), toProduct.DisplayName
	}
	n := &models.NotificationQueue{ID: uuidutil.NewV7(), CustomerID: sub.CustomerID, EventType: event, Data: data}
	if err := s.notifications.CreateAndDeliver(ctx, n); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warnf("failed to emit %s notification", event)
	}
}

// Get reads one migration with its moves counted.
func (s *PriceMigrationService) Get(ctx context.Context, id billing.PriceMigrationID) (*billing.PriceMigration, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetPriceMigration(ctx, gen.GetPriceMigrationParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPriceMigrationNotFound
	}
	if err != nil {
		return nil, err
	}
	out := priceMigrationView(row.BillingPriceMigration, row.ProductKey, row.Scheduled, row.Applied, row.Canceled, row.Blocked)
	return &out, nil
}

// List is one page of the merchant's migrations, newest first.
func (s *PriceMigrationService) List(ctx context.Context, p billing.PriceMigrationListParams) (billing.ListPage[billing.PriceMigration], error) {
	var page billing.ListPage[billing.PriceMigration]
	mid, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	if p.IDs != nil {
		rows, err := s.db.Gen(ctx).ListPriceMigrationsByIDs(ctx, gen.ListPriceMigrationsByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(p.IDs)})
		if err != nil {
			return page, err
		}
		for _, r := range rows {
			page.Items = append(page.Items, priceMigrationView(r.BillingPriceMigration, r.ProductKey, r.Scheduled, r.Applied, r.Canceled, r.Blocked))
		}
		return page, nil
	}
	if (strings.TrimSpace(p.ProductKey) == "") != (strings.TrimSpace(p.PriceKey) == "") {
		return page, apperr.Invalidf("product_key and price_key go together")
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return page, err
	}
	afterAt, afterID, err := pagination.After(p.Cursor)
	if err != nil {
		return page, err
	}
	params := gen.ListPriceMigrationsPageParams{MerchantID: mid.UUID(), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)}
	if key := strings.TrimSpace(p.PriceKey); key != "" {
		product, err := catalog.NewProductService(s.db).GetByKey(ctx, strings.TrimSpace(p.ProductKey))
		if db.IsNotFound(err) {
			return page, nil
		}
		if err != nil {
			return page, err
		}
		params.FromProductID, params.FromPriceKey = &product.ID, &key
	}
	rows, err := s.db.Gen(ctx).ListPriceMigrationsPage(ctx, params)
	if err != nil {
		return page, err
	}
	items := make([]billing.PriceMigration, 0, len(rows))
	for _, r := range rows {
		items = append(items, priceMigrationView(r.BillingPriceMigration, r.ProductKey, r.Scheduled, r.Applied, r.Canceled, r.Blocked))
	}
	return pagination.Cut(items, limit, func(m billing.PriceMigration) any {
		return pagination.TimeID{At: m.CreatedAt, ID: m.ID.UUID()}
	}), nil
}

func priceMigrationView(m gen.BillingPriceMigration, productKey *string, scheduled, applied, canceled, blocked int64) billing.PriceMigration {
	out := billing.PriceMigration{
		ID: billing.PriceMigrationID(m.ID), FromPriceID: (*billing.PriceID)(m.FromPriceID), PriceKey: m.FromPriceKey,
		ToPriceID: billing.PriceID(m.ToPriceID), EffectiveAt: m.EffectiveAt.UTC(), FallbackPolicy: billing.MigrationFallback(m.FallbackPolicy),
		Matched: int(m.SubscriptionsMatched), Skipped: int(m.SubscriptionsSkipped),
		Scheduled: int(scheduled), Applied: int(applied), Canceled: int(canceled), Blocked: int(blocked),
		CreatedAt: m.CreatedAt.UTC(), CanceledAt: m.CanceledAt,
	}
	if m.FromPriceKey != nil {
		out.ProductKey = productKey
	}
	return out
}

// Cancel cancels the migration's still-scheduled moves; applied and blocked
// ones stay, and an archived source stays archived. A Stripe subscription
// already carrying the move as a Stripe schedule is listed in
// RailReleaseRequired: that schedule is not released here.
func (s *PriceMigrationService) Cancel(ctx context.Context, id billing.PriceMigrationID) (*billing.PriceMigrationCancel, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if _, err := s.db.Gen(ctx).CancelPriceMigration(ctx, gen.CancelPriceMigrationParams{MerchantID: mid.UUID(), ID: id.UUID(), Now: s.now().UTC()}); err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListScheduledMigrationChanges(ctx, gen.ListScheduledMigrationChangesParams{MerchantID: mid.UUID(), PriceMigrationID: id.UUID()})
	if err != nil {
		return nil, err
	}
	out := &billing.PriceMigrationCancel{RailReleaseRequired: []billing.SubscriptionID{}}
	for _, change := range models.ScheduledChangesFromGen(rows) {
		var sub *models.Subscription
		err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := s.db.NewWithPgxTx(tx)
			var err error
			if sub, err = NewSubscriptionRepo(d).GetByIDForUpdate(ctx, change.SubscriptionID); err != nil {
				return err
			}
			if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
				return err
			}
			return cancelChange(ctx, d, change.ID, s.now())
		})
		if errors.Is(err, errChangeNotScheduled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out.Canceled++
		if sub.Rail == models.RailStripe && sub.CollectionPolicy != models.CollectionPolicyEngine {
			out.RailReleaseRequired = append(out.RailReleaseRequired, billing.SubscriptionID(sub.ID))
		}
	}
	migration, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	out.PriceMigration = *migration
	return out, nil
}
