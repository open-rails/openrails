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
//     rebill; a push before then would flip a period early.
//   - CCBill, Solana, an NMI schedule no declared account reaches: blocked
//     (rail_requires_user_action); the fallback policy records intent.
//
// A provider push is a price_migration_push operation recorded with the move:
// it waits for the subscription's last period before the move, retries on
// its backoff, and resolves the move (applied, skipped or blocked); a push
// the provider can never take blocks it with a finding.
//
// Bounds: creating a migration counts its subscribers and admits each as a
// destructive unit before reading any, then reaches them a page of
// migrationBatch per transaction: the first with the migration's own row,
// the rest in openrails.price_migration jobs. A preview classifies at most
// previewLimit. Its sources are archived by the catalog command (service).
// A migration's command settles when its last move resolves.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	catalogwire "github.com/open-rails/openrails/catalog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
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
	ErrRebillTermsCommitted = apperr.New(http.StatusConflict, billing.CodeRebillTermsCommitted, "accepted recurring payment owns the pending price terms")
	// ErrMembershipSlotTaken: the customer already holds a subscription of the
	// product or its tier group (or one whose provider stop is pending).
	ErrMembershipSlotTaken = errors.New("membership slot taken")
	// ErrRenewalInProgress refuses a cancel while an accepted renewal payment is
	// unresolved; the cancel succeeds once the payment resolves.
	ErrRenewalInProgress = apperr.New(http.StatusConflict, billing.CodePaymentInProgress, "a renewal payment for this subscription is unresolved; cancel again once it resolves")

	ErrPriceCurrencyMismatch       = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceChangeCurrencyMismatch, "the target price must be in the subscription's currency")
	ErrPriceTargetArchived         = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceChangeTargetArchived, "the target price is archived")
	ErrPriceIncreaseNoticeTooShort = apperr.New(http.StatusUnprocessableEntity, billing.CodePriceIncreaseNoticeTooShort, "effective_at is inside the merchant's notice window for a price increase")
	ErrPriceMigrationNotFound      = apperr.New(http.StatusNotFound, billing.CodePriceMigrationNotFound, "price migration not found")

	errMigrationPriceNotFound   = apperr.New(http.StatusNotFound, billing.CodePriceNotFound, "price not found")
	errMigrationProductNotFound = apperr.New(http.StatusNotFound, billing.CodeProductNotFound, "product not found")
	errMigrationKeyNotFound     = apperr.New(http.StatusNotFound, billing.CodePriceKeyNotFound, "no price holds this key")
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
	// NMI moves a named plan's schedule only by a plan swap.
	migrationBlockedNMINamedPlan = "nmi_named_plan"
	// An update writes plan_payments; one NMI does not state is not guessed.
	migrationBlockedNMIPaymentsUnknown = "nmi_plan_payments_unknown"
	migrationBlockedNMIScheduleMissing = "nmi_schedule_missing"
)

// MigrationPushes records a move's provider push as a durable operation in
// the caller's transaction, first tried at notBefore.
type MigrationPushes interface {
	QueuePush(ctx context.Context, d *db.DB, sub *models.Subscription, change *models.ScheduledChange, notBefore time.Time) error
}

// PriceMigrationService creates, previews, reads and cancels price
// migrations, and pushes their moves.
type PriceMigrationService struct {
	db            *db.DB
	prices        *catalog.PriceService
	subscriptions *SubscriptionService
	config        *merchantconfig.Store
	stripe        *StripeService
	nmi           *NMIPlanPusher
	pushes        MigrationPushes
	clock         clockwork.Clock
	admissions    *destructive.Admissions
	Log           *slog.Logger
}

func NewPriceMigrationService(d *db.DB, prices *catalog.PriceService, subscriptions *SubscriptionService, config *merchantconfig.Store, stripe *StripeService, nmi *NMIPlanPusher, clock clockwork.Clock) *PriceMigrationService {
	return &PriceMigrationService{db: d, prices: prices, subscriptions: subscriptions, config: config, stripe: stripe, nmi: nmi, clock: timeutil.FirstClock(clock), Log: d.Log}
}

func (s *PriceMigrationService) now() time.Time { return s.clock.Now() }

// WithDestructiveAdmissions binds the same command budget used by all staff
// changes. A migration keeps its reservation until its last move resolves.
func (s *PriceMigrationService) WithDestructiveAdmissions(a *destructive.Admissions) *PriceMigrationService {
	s.admissions = a
	return s
}

// WithPushes binds where a move's provider push is recorded.
func (s *PriceMigrationService) WithPushes(p MigrationPushes) *PriceMigrationService {
	s.pushes = p
	return s
}

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
		if plan.target, err = s.price(ctx, p.ToPriceID); err != nil {
			return nil, err
		}
		if plan.target.Archived {
			return nil, ErrPriceTargetArchived
		}
		if !plan.target.IsRecurring() {
			return nil, apperr.Invalidf("to_price_id must be a recurring price")
		}
	}
	if !byKey {
		if plan.fromPrice, err = s.price(ctx, p.FromPriceID); err != nil {
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

func (s *PriceMigrationService) price(ctx context.Context, id billing.PriceID) (*models.Price, error) {
	price, err := s.prices.GetByPublicID(ctx, id)
	if db.IsNotFound(err) {
		return nil, errMigrationPriceNotFound
	}
	return price, err
}

func sameCurrency(a, b *models.Price) bool {
	return strings.EqualFold(strings.TrimSpace(a.Currency), strings.TrimSpace(b.Currency))
}

// Bounds of a migration's work: the subscribers one transaction reaches,
// and the subscribers a preview classifies.
const (
	migrationBatch = 100
	previewLimit   = 1000
)

func (p *migrationPlan) sourceIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(p.sources))
	for id := range p.sources {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}

// cohortSize counts the plan's subscribers and finds the highest id among
// them: a walk stops there, so subscribers who join later are not moved.
func (s *PriceMigrationService) cohortSize(ctx context.Context, plan *migrationPlan) (int, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, uuid.Nil, err
	}
	q := s.db.Gen(ctx)
	sources := plan.sourceIDs()
	n, err := q.CountMigrationCohort(ctx, gen.CountMigrationCohortParams{MerchantID: mid.UUID(), PriceIds: sources})
	if err != nil || n == 0 {
		return 0, uuid.Nil, err
	}
	through, err := q.LastMigrationCohortID(ctx, gen.LastMigrationCohortIDParams{MerchantID: mid.UUID(), PriceIds: sources})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, uuid.Nil, nil
	}
	return int(n), through, err
}

// cohortCursor is where a walk of a cohort stands, in (price, id) order:
// the zero cursor, the nil ids, is before every subscriber.
type cohortCursor struct {
	PriceID uuid.UUID `json:"price_id"`
	ID      uuid.UUID `json:"id"`
}

// cohortPage is the plan's next subscribers after the cursor, up to through.
func cohortPage(ctx context.Context, d *db.DB, plan *migrationPlan, through uuid.UUID, after cohortCursor) ([]*models.Subscription, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.Gen(ctx).ListMigrationCohortPage(ctx, gen.ListMigrationCohortPageParams{MerchantID: mid.UUID(), PriceIds: plan.sourceIDs(), Through: through,
		AfterPriceID: after.PriceID, AfterID: after.ID, RowLimit: migrationBatch})
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

// gatewaySchedule: a subscription whose provider-run schedule a migration
// pushes only when the merchant declares an account reaching its PSP.
func gatewaySchedule(sub *models.Subscription) bool {
	return sub.Rail != models.RailCCBill && sub.Rail != models.RailSolana && sub.Rail != models.RailStripe && sub.CollectionPolicy != models.CollectionPolicyEngine
}

// pushability is, for each PSP of subs' gateway schedules, whether the
// merchant declares an account reaching it: decided once per PSP, before any
// transaction, since it reads the merchant's configuration. An error is
// transient and answers nothing.
func (s *PriceMigrationService) pushability(ctx context.Context, subs []*models.Subscription) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, sub := range subs {
		if _, decided := out[sub.PspID]; decided || !gatewaySchedule(sub) || strings.TrimSpace(sub.RailSubscriptionID) == "" {
			continue
		}
		reached, err := s.nmi.CanPush(ctx, sub)
		if err != nil {
			return nil, fmt.Errorf("price migration: can the PSP be pushed: %w", err)
		}
		out[sub.PspID] = reached
	}
	return out, nil
}

// capabilityOf is sub's capability given its PSP's pushability.
func capabilityOf(sub *models.Subscription, pushable map[uuid.UUID]bool) migrationCapability {
	switch {
	case sub.Rail == models.RailCCBill, sub.Rail == models.RailSolana:
		// A redirect flow or the customer's on-chain signature.
		return capabilityUserAction
	case sub.CollectionPolicy == models.CollectionPolicyEngine:
		return capabilityEngine
	case sub.Rail == models.RailStripe:
		return capabilityAutoStripe
	case pushable[sub.PspID] && strings.TrimSpace(sub.RailSubscriptionID) != "":
		// A gateway schedule keeps charging its own amount, so it is pushed
		// even when a stored credential exists.
		return capabilityAutoNMI
	default:
		return capabilityUserAction
	}
}

func (s *PriceMigrationService) capability(ctx context.Context, sub *models.Subscription) (migrationCapability, error) {
	pushable, err := s.pushability(ctx, []*models.Subscription{sub})
	return capabilityOf(sub, pushable), err
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

// Bounds of a calendar month, in hours.
const (
	monthMinHours = 28 * 24
	monthMaxHours = 31 * 24
)

// cadence is a recurring price's billing interval: hours, else months.
func cadence(p *models.Price) (n int64, months, ok bool) {
	switch {
	case p.BillingIntervalHours != nil && *p.BillingIntervalHours > 0:
		return int64(*p.BillingIntervalHours), false, true
	case p.BillingIntervalMonths != nil && *p.BillingIntervalMonths > 0:
		return int64(*p.BillingIntervalMonths), true, true
	}
	return 0, false, false
}

// costRises reports whether a renewal at to bills sub more per unit of time
// than one at from: amount × the seats each renewal bills, over its cadence,
// compared exactly. Hours against months take a month at its least
// favourable length; a price without a cadence counts as a rise.
func costRises(sub *models.Subscription, from, to *models.Price) bool {
	fromN, fromMonths, ok := cadence(from)
	toN, toMonths, toOK := cadence(to)
	if !ok || !toOK {
		return true
	}
	fromHi, toLo := big.NewInt(fromN), big.NewInt(toN)
	if fromMonths != toMonths {
		if fromMonths {
			fromHi.Mul(fromHi, big.NewInt(monthMaxHours))
		}
		if toMonths {
			toLo.Mul(toLo, big.NewInt(monthMinHours))
		}
	}
	cost := func(p *models.Price) *big.Int {
		seats := SeatCount(renewalQuantity(sub, p, sub.Quantity))
		return new(big.Int).Mul(big.NewInt(p.Amount), big.NewInt(int64(seats)))
	}
	// to/toLo > from/fromHi, cross-multiplied.
	return new(big.Int).Mul(cost(to), fromHi).Cmp(new(big.Int).Mul(cost(from), toLo)) > 0
}

// noticeDays is the merchant's notice window for a price increase.
func (s *PriceMigrationService) noticeDays(ctx context.Context) (int, error) {
	days := DefaultPriceIncreaseNoticeDays
	if s.config != nil {
		cfg, _, err := s.config.Get(ctx)
		if err != nil {
			return 0, fmt.Errorf("price migration: notice window: %w", err)
		}
		if cfg.RepriceNoticeWindowDays != nil {
			days = *cfg.RepriceNoticeWindowDays
		}
	}
	return days, nil
}

// noticeViolated: a move that raises what sub pays per unit of time, with
// effective_at inside a notice window of days. Equal or lower cost never is.
func (s *PriceMigrationService) noticeViolated(sub *models.Subscription, from, to *models.Price, effectiveAt time.Time, days int) bool {
	return costRises(sub, from, to) && days > 0 && effectiveAt.Before(s.now().Add(time.Duration(days)*24*time.Hour))
}

type classified struct {
	sub          *models.Subscription
	from         *models.Price
	capability   migrationCapability
	acknowledged bool
	outcome      billing.PriceMigrationOutcome
}

// classify decides each subscription's disposition without writing, counting
// it into byRail. noticeDays is the merchant's notice window.
func (s *PriceMigrationService) classify(ctx context.Context, plan *migrationPlan, cohort []*models.Subscription, noticeDays int, byRail map[string]*billing.PriceMigrationRailCounts) ([]*classified, error) {
	ids := make([]uuid.UUID, 0, len(cohort))
	for _, sub := range cohort {
		ids = append(ids, sub.ID)
	}
	pending, err := PendingChanges(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	pushable, err := s.pushability(ctx, cohort)
	if err != nil {
		return nil, err
	}
	out := make([]*classified, 0, len(cohort))
	for _, sub := range cohort {
		c := &classified{sub: sub, from: plan.sources[sub.PriceID], capability: capabilityOf(sub, pushable)}
		c.outcome = billing.PriceMigrationOutcome{SubscriptionID: billing.SubscriptionID(sub.ID), Rail: billing.Rail(sub.Rail)}
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
		case c.capability == capabilityEngine && (c.from.BillingIntervalMonths != nil || plan.target.BillingIntervalMonths != nil):
			block("calendar subscription plan changes are not supported")
		case c.capability == capabilityUserAction:
			block(migrationBlockedRailUserAction)
		case c.capability == capabilityAutoNMI && !nmiCyclesCompatible(c.from, plan.target):
			block(migrationBlockedNMIIntervalMismatch)
		case c.capability == capabilityAutoNMI && !railMinorRepresentable(plan.target.Currency, plan.target.Amount):
			block(migrationBlockedNMISubCentAmount)
		case c.capability == capabilityAutoStripe && (!stripeMapped(c.from) || !stripeMapped(plan.target)):
			block(migrationBlockedMissingRailPrice)
		default:
			violated := s.noticeViolated(sub, c.from, plan.target, plan.effectiveAt, noticeDays)
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
	return out, nil
}

// Preview is what Create would do, with nothing written: every subscriber
// counted, and the first previewLimit of them classified.
func (s *PriceMigrationService) Preview(ctx context.Context, p billing.CreatePriceMigrationParams) (*billing.PriceMigrationPreview, error) {
	plan, err := s.resolve(ctx, p, true)
	if err != nil {
		return nil, err
	}
	matched, through, err := s.cohortSize(ctx, plan)
	if err != nil {
		return nil, err
	}
	out := &billing.PriceMigrationPreview{EffectiveAt: plan.effectiveAt, Matched: matched, ByRail: map[string]*billing.PriceMigrationRailCounts{}, Outcomes: []billing.PriceMigrationOutcome{}}
	if plan.target == nil || matched == 0 {
		return out, nil
	}
	target := plan.target.PublicID
	out.ToPriceID = &target
	days, err := s.noticeDays(ctx)
	if err != nil {
		return nil, err
	}
	var after cohortCursor
	for out.Classified < previewLimit {
		page, err := cohortPage(ctx, s.db, plan, through, after)
		if err != nil {
			return nil, err
		}
		classes, err := s.classify(ctx, plan, page, days, out.ByRail)
		if err != nil {
			return nil, err
		}
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
		out.Classified += len(page)
		if len(page) < migrationBatch {
			break
		}
		last := page[len(page)-1]
		after = cohortCursor{PriceID: last.PriceID, ID: last.ID}
	}
	return out, nil
}

// PendingMigration is a migration admitted and not yet recorded. Its
// command's units are the subscribers counted and the sources it archives;
// the catalog archives Archive under Action before Record.
type PendingMigration struct {
	plan   *migrationPlan
	Action *destructive.Action
	// Archive are the sources still on sale when it was admitted.
	Archive []billing.PriceID
}

// Admit resolves a migration and admits it as one destructive command, a
// unit for each subscriber counted and each source it archives, before any
// is read. prior is the migration the same request already recorded.
func (s *PriceMigrationService) Admit(ctx context.Context, p billing.CreatePriceMigrationParams) (*PendingMigration, *billing.PriceMigration, error) {
	plan, err := s.resolve(ctx, p, false)
	if err != nil {
		return nil, nil, err
	}
	pending := &PendingMigration{plan: plan}
	if plan.archive {
		for _, id := range plan.sourceIDs() {
			if source := plan.sources[id]; !source.Archived {
				pending.Archive = append(pending.Archive, source.PublicID)
			}
		}
	}
	matched, through, err := s.cohortSize(ctx, plan)
	if err != nil {
		return nil, nil, err
	}
	var prior *billing.PriceMigration
	pending.Action, prior, err = s.admitMigration(ctx, p, plan, matched+len(pending.Archive), through)
	return pending, prior, err
}

// Record counts the cohort again, its sources no longer selling, so no one
// who bought since is left behind, then records the migration and reaches
// its first page in one transaction; a job reaches each further page. A
// concurrent identical request answers the migration the first recorded.
func (s *PriceMigrationService) Record(ctx context.Context, pending *PendingMigration) (*billing.PriceMigration, error) {
	plan := pending.plan
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	matched, through, err := s.cohortSize(ctx, plan)
	if err != nil {
		return nil, err
	}
	run := &migrationRun{plan: plan, through: through, action: pending.Action}
	if run.noticeDays, err = s.noticeDays(ctx); err != nil {
		return nil, err
	}
	if run.targetProduct, err = catalog.NewProductService(s.db).GetByID(ctx, plan.target.ProductID); err != nil {
		return nil, fmt.Errorf("price migration: target product: %w", err)
	}
	page, err := s.readPage(ctx, run, cohortCursor{})
	if err != nil {
		return nil, err
	}
	var migration uuid.UUID
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		params := gen.CreatePriceMigrationParams{MerchantID: mid.UUID(), ToPriceID: plan.target.ID, EffectiveAt: plan.effectiveAt, FallbackPolicy: string(plan.fallback), SubscriptionsMatched: int32(matched)} // #nosec G115 -- counted subscribers
		if plan.fromPrice != nil {
			params.FromPriceID = &plan.fromPrice.ID
		} else {
			params.FromProductID, params.FromPriceKey = &plan.product.ID, &plan.key
		}
		if pending.Action != nil {
			// The command's row orders identical requests: the second finds
			// the first's migration.
			if _, err := q.LockDestructiveAction(ctx, gen.LockDestructiveActionParams{MerchantID: mid.UUID(), ID: pending.Action.ID}); err != nil {
				return err
			}
			recorded, err := q.GetPriceMigrationByDestructiveAction(ctx, gen.GetPriceMigrationByDestructiveActionParams{MerchantID: mid.UUID(), DestructiveActionID: pending.Action.ID})
			if err == nil {
				migration = recorded.ID
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			params.DestructiveActionID = &pending.Action.ID
		}
		row, err := q.CreatePriceMigration(ctx, params)
		if err != nil {
			return fmt.Errorf("price migration: create: %w", err)
		}
		migration, run.migration = row.ID, row.ID
		return s.reachPage(ctx, tx, run, page)
	})
	if err != nil {
		return nil, err
	}
	s.Log.InfoContext(ctx, "price migration recorded", "price_migration_id", migration, "to_price_id", plan.target.ID, "effective_at", plan.effectiveAt, "matched", matched)
	return s.Get(ctx, billing.PriceMigrationID(migration))
}

// migrationRun is what reaching a migration's subscribers needs.
type migrationRun struct {
	migration     uuid.UUID
	action        *destructive.Action
	plan          *migrationPlan
	targetProduct *models.Product
	through       uuid.UUID
	noticeDays    int
	canceled      bool
}

// migrationPage is a page of a cohort, read and classified before its
// transaction: classifying reads the merchant's configuration.
type migrationPage struct {
	subs    []*models.Subscription
	classes []*classified
}

func (s *PriceMigrationService) readPage(ctx context.Context, run *migrationRun, after cohortCursor) (*migrationPage, error) {
	page := &migrationPage{}
	if run.through == uuid.Nil {
		return page, nil
	}
	var err error
	if page.subs, err = cohortPage(ctx, s.db, run.plan, run.through, after); err != nil {
		return nil, err
	}
	page.classes, err = s.classify(ctx, run.plan, page.subs, run.noticeDays, map[string]*billing.PriceMigrationRailCounts{})
	return page, err
}

// reachPage records the page in tx, the migration's row held: each
// subscriber not reached before gets its move, blocked or scheduled with
// its notice and push. Then it queues the next page, or, after the last or
// once canceled, ends the walk.
func (s *PriceMigrationService) reachPage(ctx context.Context, tx pgx.Tx, run *migrationRun, page *migrationPage) error {
	d := s.db.NewWithPgxTx(tx)
	mid := run.plan.target.MerchantID
	if run.canceled {
		return s.finishWalk(ctx, tx, mid, run.migration, run.action)
	}
	ids := make([]uuid.UUID, 0, len(page.subs))
	for _, sub := range page.subs {
		ids = append(ids, sub.ID)
	}
	moved, err := d.Gen(ctx).ListMigrationMoves(ctx, gen.ListMigrationMovesParams{MerchantID: mid, PriceMigrationID: run.migration, SubscriptionIds: ids})
	if err != nil {
		return err
	}
	for _, c := range page.classes {
		// One that moved to another source is reached once.
		if !slices.Contains(moved, c.sub.ID) {
			if err := s.reach(ctx, d, run, c); err != nil {
				return err
			}
		}
	}
	if len(page.subs) < migrationBatch {
		return s.finishWalk(ctx, tx, mid, run.migration, run.action)
	}
	last := page.subs[len(page.subs)-1]
	next := PriceMigrationArgs{MerchantID: mid, MigrationID: run.migration, Sources: run.plan.sourceIDs(), Through: run.through,
		Acknowledge: run.plan.acknowledge, After: cohortCursor{PriceID: last.PriceID, ID: last.ID}}
	opts := next.InsertOpts()
	_, err = d.InsertRiverJob(ctx, next, &opts)
	return err
}

// finishWalk ends a migration's walk in tx: subscribers it made no move for
// count as skipped, and its command's preparation ends, settling it once no
// move waits. A walk already ended is left as it is.
func (s *PriceMigrationService) finishWalk(ctx context.Context, tx pgx.Tx, mid, migration uuid.UUID, action *destructive.Action) error {
	q := s.db.NewWithPgxTx(tx).Gen(ctx)
	if _, err := q.SetPriceMigrationSkipped(ctx, gen.SetPriceMigrationSkippedParams{MerchantID: mid, ID: migration}); err != nil {
		return err
	}
	if action == nil {
		return nil
	}
	locked, err := q.LockDestructiveAction(ctx, gen.LockDestructiveActionParams{MerchantID: mid, ID: action.ID})
	if err != nil {
		return err
	}
	if locked.State != string(destructive.StateStarted) {
		return nil
	}
	if err := s.admissions.FinishTx(ctx, tx, mid, action.ID, destructive.OutcomeUncertain, "migration:"+migration.String()+":prepared"); err != nil {
		return err
	}
	// Every move may already have resolved.
	return q.SettlePriceMigration(ctx, gen.SettlePriceMigrationParams{MerchantID: mid, PriceMigrationID: migration, Now: s.now().UTC()})
}

// reach records one classified subscriber's move under its lock: blocked, or
// scheduled with its notice and provider push. One that left the source or
// stopped billing since it was read, or whose move is refused, is skipped,
// its savepoint undone.
func (s *PriceMigrationService) reach(ctx context.Context, d *db.DB, run *migrationRun, c *classified) error {
	if c.outcome.Disposition != billing.MigrationBlocked && c.outcome.Disposition != billing.MigrationScheduled {
		return nil
	}
	pushed := c.outcome.Disposition == billing.MigrationScheduled && (c.capability == capabilityAutoStripe || c.capability == capabilityAutoNMI)
	if pushed && s.pushes == nil {
		return errors.New("price migration: provider pushes are not wired")
	}
	target, effectiveAt := run.plan.target, run.plan.effectiveAt
	err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := d.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, c.sub.ID)
		if err != nil {
			return err
		}
		if sub.PriceID != c.from.ID || !migratable(sub) {
			return errChangeNotScheduled
		}
		if c.outcome.Disposition == billing.MigrationBlocked {
			_, err := d.Gen(ctx).CreateBlockedScheduledChange(ctx, gen.CreateBlockedScheduledChangeParams{
				MerchantID: sub.MerchantID, SubscriptionID: sub.ID, FromPriceID: sub.PriceID, PriceID: target.ID,
				EffectiveAt: effectiveAt, PriceMigrationID: run.migration, BlockedReason: *c.outcome.Reason,
			})
			return err
		}
		change, err := ScheduleChange(ctx, d, sub, NewScheduledChange{PriceID: target.ID, EffectiveAt: effectiveAt, Source: billing.ScheduledChangeMigration, PriceMigrationID: &run.migration, AcknowledgedShortNotice: c.acknowledged}, s.now())
		if err != nil {
			return err
		}
		if err := s.notify(ctx, d, change, sub, c.from, target, run.targetProduct, effectiveAt); err != nil {
			return err
		}
		if !pushed {
			return nil
		}
		notBefore := s.now()
		if deferredPushRequired(effectiveAt, sub) {
			notBefore = *sub.CurrentPeriodEndsAt
		}
		return s.pushes.QueuePush(ctx, d, sub, change, notBefore)
	})
	var refused *apperr.Error
	if errors.Is(err, errChangeNotScheduled) || errors.As(err, &refused) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("price migration: reach %s: %w", c.sub.ID, err)
	}
	return nil
}

// migratable: a subscription a migration's cohort holds.
func migratable(sub *models.Subscription) bool {
	return sub.Status == models.StatusActive || sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod
}

// RunPage reaches one queued page of a migration: read and classified first,
// then recorded in one transaction with the job's completion (complete), so
// a page is reached once. A migration gone with its merchant is done.
func (s *PriceMigrationService) RunPage(ctx context.Context, args PriceMigrationArgs, complete func(context.Context, pgx.Tx) error) error {
	ctx = merchant.WithID(ctx, billing.MerchantID(args.MerchantID))
	run, err := s.loadRun(ctx, args)
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	page, err := s.readPage(ctx, run, args.After)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		locked, err := s.db.NewWithPgxTx(tx).Gen(ctx).LockPriceMigration(ctx, gen.LockPriceMigrationParams{MerchantID: args.MerchantID, ID: args.MigrationID})
		if db.IsNotFound(err) {
			return complete(ctx, tx)
		}
		if err != nil {
			return err
		}
		run.canceled = locked.CanceledAt != nil
		if err := s.reachPage(ctx, tx, run, page); err != nil {
			return err
		}
		return complete(ctx, tx)
	})
}

// loadRun is a queued page's run: the migration and its admitted terms.
func (s *PriceMigrationService) loadRun(ctx context.Context, args PriceMigrationArgs) (*migrationRun, error) {
	row, err := s.db.Gen(ctx).GetPriceMigration(ctx, gen.GetPriceMigrationParams{MerchantID: args.MerchantID, ID: args.MigrationID})
	if err != nil {
		return nil, err
	}
	m := row.BillingPriceMigration
	plan := &migrationPlan{sources: map[uuid.UUID]*models.Price{}, effectiveAt: m.EffectiveAt.UTC(), acknowledge: args.Acknowledge}
	if plan.target, err = s.prices.GetByID(ctx, m.ToPriceID); err != nil {
		return nil, err
	}
	for _, id := range args.Sources {
		if plan.sources[id], err = s.prices.GetByID(ctx, id); err != nil {
			return nil, err
		}
	}
	run := &migrationRun{migration: m.ID, plan: plan, through: args.Through}
	if run.targetProduct, err = catalog.NewProductService(s.db).GetByID(ctx, plan.target.ProductID); err != nil {
		return nil, err
	}
	if run.noticeDays, err = s.noticeDays(ctx); err != nil {
		return nil, err
	}
	if m.DestructiveActionID != nil {
		action, err := s.admissions.GetAction(ctx, m.MerchantID, *m.DestructiveActionID)
		if err != nil {
			return nil, err
		}
		run.action = &action
	}
	return run, nil
}

// MigrationStoppedFindingType is the finding of a walk whose page could
// never be reached: the subscribers left were not moved.
const MigrationStoppedFindingType = "life.price_migration.stopped"

// StopWalk ends the walk of a page job out of attempts: the subscribers it
// never reached count as skipped, the command's preparation ends, and a
// finding tells the operator, so the migration is not left preparing.
func (s *PriceMigrationService) StopWalk(ctx context.Context, args PriceMigrationArgs, cause error) error {
	ctx = merchant.WithID(ctx, billing.MerchantID(args.MerchantID))
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		m, err := d.Gen(ctx).LockPriceMigration(ctx, gen.LockPriceMigrationParams{MerchantID: args.MerchantID, ID: args.MigrationID})
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var action *destructive.Action
		if m.DestructiveActionID != nil {
			action = &destructive.Action{ID: *m.DestructiveActionID, MerchantID: m.MerchantID}
		}
		if err := s.finishWalk(ctx, tx, m.MerchantID, m.ID, action); err != nil {
			return err
		}
		public := billing.PriceMigrationID(m.ID).String()
		evidence, err := json.Marshal(map[string]any{"price_migration_id": public, "cause": cause.Error()})
		if err != nil {
			return err
		}
		remedy := "A price migration stopped reaching its subscribers after its page failed every attempt; those it had not reached keep their price. Fix the cause, then migrate them again."
		row, err := d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: m.MerchantID, FindingType: MigrationStoppedFindingType,
			SubjectKey: public, Severity: "high", Status: "requires_review", RecommendedAction: &remedy, Evidence: evidence})
		if err != nil {
			return err
		}
		if err := alertjob.Notify(ctx, d, alertjob.FindingOf(row), s.now()); err != nil && ctx.Err() == nil {
			s.Log.WarnContext(ctx, "alerting the operator of a stopped price migration failed; the finding stays in the console", logx.Err(err), "price_migration_id", m.ID)
		}
		return nil
	})
}

// KindPriceMigration is the job that reaches a migration's subscribers past
// its first page, a page per job.
const KindPriceMigration = "openrails.price_migration"

// PriceMigrationArgs is one page of a migration: its subscribers on Sources
// after After, up to Through.
type PriceMigrationArgs struct {
	MerchantID  uuid.UUID    `json:"merchant_id"`
	MigrationID uuid.UUID    `json:"price_migration_id"`
	Sources     []uuid.UUID  `json:"sources"`
	Through     uuid.UUID    `json:"through"`
	Acknowledge bool         `json:"acknowledge_short_notice"`
	After       cohortCursor `json:"after"`
}

func (PriceMigrationArgs) Kind() string { return KindPriceMigration }

func (PriceMigrationArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 25}
}

// stripeMapped reports whether a price names its Stripe price.
func stripeMapped(p *models.Price) bool {
	id, ok := p.GetStripeConfig()
	return ok && strings.TrimSpace(id) != ""
}

// push performs the rail's part of one scheduled move.
func (s *PriceMigrationService) push(ctx context.Context, capability migrationCapability, sub *models.Subscription, source, target *models.Price, targetProduct *models.Product, change *models.ScheduledChange) error {
	switch capability {
	case capabilityEngine:
		return nil
	case capabilityAutoStripe:
		return s.pushStripe(ctx, sub, source, target, change)
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
		return refusePush(migrationBlockedRailPushFailed, "subscription missing nmi reference")
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return refusePush(migrationBlockedRailPushFailed, "subscription missing current period end")
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
// period end (no proration, no anchor move, same seats), idempotent on the
// move; converge applies the change from Stripe's truth.
func (s *PriceMigrationService) pushStripe(ctx context.Context, sub *models.Subscription, source, target *models.Price, change *models.ScheduledChange) error {
	if s.stripe == nil {
		return fmt.Errorf("stripe pusher not configured")
	}
	targetStripeID, ok := target.GetStripeConfig()
	if !ok || strings.TrimSpace(targetStripeID) == "" {
		return refusePush(migrationBlockedMissingRailPrice, "")
	}
	sourceStripeID, ok := source.GetStripeConfig()
	if !ok || strings.TrimSpace(sourceStripeID) == "" {
		return refusePush(migrationBlockedMissingRailPrice, "")
	}
	railSubID := strings.TrimSpace(sub.RailSubscriptionID)
	if railSubID == "" {
		return refusePush(migrationBlockedRailPushFailed, "subscription missing stripe reference")
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return refusePush(migrationBlockedRailPushFailed, "subscription missing current period end")
	}
	if deferredPushRequired(change.EffectiveAt, sub) {
		return fmt.Errorf("stripe_deferred_push_required: effective_at %s is beyond the current period end %s", change.EffectiveAt.UTC().Format(time.RFC3339), sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339))
	}
	periodStart := sub.StartedAt
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		periodStart = *sub.CurrentPeriodStartsAt
	}
	return s.stripe.ScheduleSubscriptionPriceChange(ctx, change.ID.String(), railSubID, sourceStripeID, targetStripeID, periodStart, *sub.CurrentPeriodEndsAt, target.RecurringCycleDays())
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

// notify records the schedule transition in its transaction: a plan change when the product
// changes (the legal content differs), else a price change.
func (s *PriceMigrationService) notify(ctx context.Context, d *db.DB, change *models.ScheduledChange, sub *models.Subscription, from, to *models.Price, toProduct *models.Product, effectiveAt time.Time) error {
	effective := effectiveAt.UTC()
	data := billing.NotificationData{
		SubscriptionID: billing.SubscriptionID(sub.ID), FromPriceID: from.PublicID, ToPriceID: to.PublicID,
		OldAmount: &from.Amount, NewAmount: &to.Amount, Currency: catalogwire.Currency(to.Currency), EffectiveAt: &effective,
	}
	if change.PriceMigrationID != nil {
		data.PriceMigrationID = billing.PriceMigrationID(*change.PriceMigrationID)
	}
	event := models.NotificationSubscriptionRepriceScheduled
	if to.ProductID != from.ProductID {
		event = models.NotificationSubscriptionPlanChangeScheduled
		data.ToProductID, data.ToProductName = billing.ProductID(toProduct.ID), toProduct.DisplayName
	}
	n := &models.NotificationEvent{ID: uuid.NewSHA1(change.ID, []byte("scheduled-notification")), CustomerID: sub.CustomerID, EventType: event, Data: data, CreatedAt: s.now()}
	return NewNotificationService(d).CreateIfAbsent(ctx, n)
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
	out := priceMigrationView(row.BillingPriceMigration, row.FromPricePublicID, row.ToPricePublicID, row.ProductKey, row.Scheduled, row.Applied, row.Canceled, row.Blocked, row.Skipped, row.Preparing)
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
			page.Items = append(page.Items, priceMigrationView(r.BillingPriceMigration, r.FromPricePublicID, r.ToPricePublicID, r.ProductKey, r.Scheduled, r.Applied, r.Canceled, r.Blocked, r.Skipped, r.Preparing))
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
		items = append(items, priceMigrationView(r.BillingPriceMigration, r.FromPricePublicID, r.ToPricePublicID, r.ProductKey, r.Scheduled, r.Applied, r.Canceled, r.Blocked, r.Skipped, r.Preparing))
	}
	return pagination.Cut(items, limit, func(m billing.PriceMigration) any {
		return pagination.TimeID{At: m.CreatedAt, ID: m.ID.UUID()}
	}), nil
}

// priceMigrationView counts a move skipped after creation with the
// subscriptions skipped at it.
func priceMigrationView(m gen.BillingPriceMigration, fromID *string, toID string, productKey *string, scheduled, applied, canceled, blocked, skipped int64, preparing bool) billing.PriceMigration {
	out := billing.PriceMigration{
		ID: billing.PriceMigrationID(m.ID), FromPriceID: (*billing.PriceID)(fromID), PriceKey: m.FromPriceKey,
		ToPriceID: billing.PriceID(toID), EffectiveAt: m.EffectiveAt.UTC(), FallbackPolicy: billing.MigrationFallback(m.FallbackPolicy),
		Matched: int(m.SubscriptionsMatched), Skipped: int(m.SubscriptionsSkipped) + int(skipped),
		Scheduled: int(scheduled), Applied: int(applied), Canceled: int(canceled), Blocked: int(blocked),
		CreatedAt: m.CreatedAt.UTC(), CanceledAt: m.CanceledAt, Preparing: preparing, Warnings: []string{},
	}
	if m.FromPriceKey != nil {
		out.ProductKey = productKey
	}
	return out
}

// Cancel cancels the migration's still-scheduled moves and their pushes, and
// ends its walk of subscribers not yet reached; resolved moves stay, and an
// archived source stays archived. A Stripe
// subscription already carrying the move as a Stripe schedule is listed in
// RailReleaseRequired: that schedule is not released here. Canceling keeps
// subscriptions where they are, so it spends no destructive unit and no hold
// refuses it. The migration's own admission settles with its last move.
func (s *PriceMigrationService) Cancel(ctx context.Context, id billing.PriceMigrationID) (*billing.PriceMigrationCancel, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	// Canceled first, ending the walk: a page reaching subscribers holds the
	// row, so once this commits no page makes another move, the list sees
	// every one made, and a walk whose next page never runs is not left
	// preparing.
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.CancelPriceMigration(ctx, gen.CancelPriceMigrationParams{MerchantID: mid.UUID(), ID: id.UUID(), Now: s.now().UTC()}); err != nil {
			return err
		}
		m, err := q.LockPriceMigration(ctx, gen.LockPriceMigrationParams{MerchantID: mid.UUID(), ID: id.UUID()})
		if err != nil {
			return err
		}
		var action *destructive.Action
		if m.DestructiveActionID != nil {
			action = &destructive.Action{ID: *m.DestructiveActionID, MerchantID: m.MerchantID}
		}
		return s.finishWalk(ctx, tx, m.MerchantID, m.ID, action)
	})
	if err != nil {
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
