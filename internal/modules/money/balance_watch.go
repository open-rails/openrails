package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A balance watch is the one stored instant per watched balance (every
// prepaid one, and every one with a running charge): when it is next looked
// at. A change in what the customer may spend marks it due (the ledger's
// ApplyIdempotent, an admission's hold), and the watcher job looks at the due
// ones. Time alone moves a balance only through a running charge's accrual and
// a credit lot's lapse, and a look schedules both.

const (
	// defaultRunwayHours is the built-in runway line: it scales with the
	// customer's spend and stays silent for an idle account.
	defaultRunwayHours = 24
	// usageRateTimeConstant is the usage rate's time constant.
	usageRateTimeConstant = time.Hour
)

// lowBalanceLine is the line a balance is judged at, each part resolved
// customer, merchant, built in.
type lowBalanceLine struct {
	amount      int64
	runwayHours int
	source      billing.LowBalanceLineSource
}

func resolveLowBalanceLine(cfg models.BalanceNoticeConfiguration, settings *gen.BillingMoneySetting, currency string) lowBalanceLine {
	line := lowBalanceLine{runwayHours: defaultRunwayHours, source: billing.LowBalanceLineDefault}
	if amount, ok := cfg.LowAmounts[currency]; ok {
		line.amount, line.source = amount, billing.LowBalanceLineMerchant
	}
	if cfg.RunwayHours != nil {
		line.runwayHours, line.source = *cfg.RunwayHours, billing.LowBalanceLineMerchant
	}
	if settings != nil {
		if settings.LowBalanceAmount != nil {
			line.amount, line.source = *settings.LowBalanceAmount, billing.LowBalanceLineCustomer
		}
		if settings.LowBalanceRunwayHours != nil {
			line.runwayHours, line.source = int(*settings.LowBalanceRunwayHours), billing.LowBalanceLineCustomer
		}
	}
	return line
}

// at is the effective line at a burn rate: the larger of the amount line and
// the rate times the runway.
func (l lowBalanceLine) at(burnPerHour int64) int64 {
	return max(l.amount, satMul(burnPerHour, int64(l.runwayHours)))
}

func (l lowBalanceLine) effective(currency string) billing.EffectiveLowBalanceLine {
	return billing.EffectiveLowBalanceLine{Currency: currency, Amount: l.amount, RunwayHours: l.runwayHours, Source: l.source}
}

// satMul multiplies two nonnegative amounts, saturating.
func satMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b)) // #nosec G115 -- both are positive
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo) // #nosec G115 -- checked above
}

// balanceNotices is the merchant's default lines; a deployment with no
// merchant configuration bound has none.
func balanceNotices(ctx context.Context, d *db.DB) (models.BalanceNoticeConfiguration, error) {
	cfg, _, err := merchantconfig.NewStore(d).Get(ctx)
	if err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
		return models.BalanceNoticeConfiguration{}, err
	}
	return cfg.BalanceNotices, nil
}

// balanceView is one balance as a look or a read sees it, at asOf.
type balanceView struct {
	asOf    time.Time
	charges []gen.BillingRunningCharge
	// settings is the customer's money settings row; nil with none.
	settings    *gen.BillingMoneySetting
	arrears     bool
	accrualRate int64
	owed        int64
	// floor is the available balance at which capacity is zero; spendFloor
	// the one at which what the customer may spend before any overdraft is.
	floor, spendFloor int64
	// available is the balance less what is held, less what lapsed lots the
	// expiry sweep has not reached will expire with.
	available int64
	lots      []creditLot
	// checkpointDue: a charge is not yet posted through the last 00:00 UTC.
	checkpointDue bool
}

// capacityFloors are the floors of a balance: capacity is zero at floor
// (prepaid, what it owes less the overdraft; arrears, as far below zero as its
// credit line still reaches), and what it may spend before any overdraft at
// spendFloor (prepaid, what it owes; arrears, the floor).
func capacityFloors(settings *gen.BillingMoneySetting, owed, overdraft int64) (floor, spendFloor int64, arrears bool) {
	if settings != nil && settings.BillingMode == BillingModeArrears && settings.CollectionRevokedAt == nil {
		floor = -max(satSub(settings.CreditLimitAmount, owed), 0)
		return floor, floor, true
	}
	return satSub(owed, overdraft), owed, false
}

func (s *MoneyService) viewBalance(ctx context.Context, q *gen.Queries, bal *models.MoneyBalance) (balanceView, error) {
	v := balanceView{asOf: bal.AsOf}
	charges, err := q.ListRunningChargesByCustomer(ctx, gen.ListRunningChargesByCustomerParams{
		MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency,
	})
	if err != nil {
		return v, err
	}
	v.charges = charges
	lines := runningLines(charges)
	v.accrualRate = lines.ratePerHour
	for _, c := range charges {
		v.checkpointDue = v.checkpointDue || c.PostedThroughAt.Before(bal.AsOf.Truncate(24*time.Hour))
	}
	if v.owed, err = s.moneyLedger(q, bal.MerchantID).OutstandingOwed(ctx, bal.CustomerID, bal.Currency); err != nil {
		return v, err
	}
	settings, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency})
	switch {
	case err == nil:
		v.settings = &settings
	case !errors.Is(err, pgx.ErrNoRows):
		return v, err
	}
	v.floor, v.spendFloor, v.arrears = capacityFloors(v.settings, v.owed, lines.overdraft)
	rows, err := q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency, AsOf: bal.AsOf,
	})
	if err != nil {
		return v, err
	}
	v.lots = make([]creditLot, len(rows))
	for i, row := range rows {
		v.lots[i] = creditLot{remaining: row.Remaining, endsAt: row.EndsAt}
	}
	// A lapsed lot the expiry sweep has not reached is still in the balance:
	// what it will expire with is not there to draw.
	lapsed, err := q.ListLapsedCreditLots(ctx, gen.ListLapsedCreditLotsParams{
		MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency, AsOf: bal.AsOf,
	})
	if err != nil {
		return v, err
	}
	unspent, err := lapsedUnspent(charges, lapsed)
	if err != nil {
		return v, err
	}
	v.available = satSub(satSub(bal.Balance, bal.HeldBalance), unspent)
	return v, nil
}

// balanceProjection is what a balance does next.
type balanceProjection struct {
	// depletesAt is when capacity reaches zero at the running charges' rate
	// and the lots' lapses, exact; nil when neither moves it there.
	depletesAt *time.Time
	depleted   bool
	// line is the effective low line at the burn rate; 0 is none. lowAt is
	// the exact instant the balance goes under it, nil when nothing exact
	// moves it there.
	line  int64
	lowAt *time.Time
	low   bool
	// runwayEndsAt is when capacity reaches zero at the burn rate; nil at
	// rate zero.
	runwayEndsAt *time.Time
}

// exactAt is the first instant the balance is at or under target as the
// running charges and the lots' lapses move it; nil when they never do.
func (v balanceView) exactAt(target int64) *time.Time {
	at := firstInstantAtOrUnder(v.asOf, v.available, v.lots, v.accrualRate, target)
	if v.accrualRate == 0 && at.Equal(v.asOf.Add(balanceHorizon)) {
		return nil
	}
	return &at
}

func (v balanceView) project(line lowBalanceLine, usageRate int64) balanceProjection {
	var p balanceProjection
	p.depletesAt = v.exactAt(v.floor)
	p.depleted = p.depletesAt != nil && !p.depletesAt.After(v.asOf)
	burn := satAdd(v.accrualRate, usageRate)
	if p.line = line.at(burn); p.line > 0 {
		// Under the line: at or under one unit less than it.
		p.lowAt = v.exactAt(satAdd(v.spendFloor, p.line-1))
		p.low = p.lowAt != nil && !p.lowAt.After(v.asOf)
	}
	if burn > 0 {
		at := firstInstantAtOrUnder(v.asOf, v.available, v.lots, burn, v.floor)
		p.runwayEndsAt = &at
	}
	return p
}

// next is when to look again: the next 00:00 UTC while a charge runs, and
// each armed crossing an exact cause moves the balance to. Nil: only a mark.
func (v balanceView) next(p balanceProjection, lowArmed, depletedArmed bool) *time.Time {
	var next *time.Time
	consider := func(at *time.Time) {
		if at != nil && (next == nil || at.Before(*next)) {
			t := *at
			next = &t
		}
	}
	if len(v.charges) > 0 {
		midnight := nextMidnight(v.asOf)
		consider(&midnight)
	}
	if lowArmed {
		consider(p.lowAt)
	}
	if depletedArmed {
		consider(p.depletesAt)
	}
	return next
}

// watched reports whether the balance keeps a watch: every prepaid one, and
// every one with a running charge.
func (v balanceView) watched() bool { return !v.arrears || len(v.charges) > 0 }

// sampleUsageRate adds the usage dated since the last sample to the decayed
// rate. Each event is read by one look; events dated before the last sample,
// or more than one time constant ago, are left out: the rate is an estimate.
func sampleUsageRate(ctx context.Context, q *gen.Queries, watch gen.BillingBalanceWatch, now time.Time) (rate int64, sampledAt time.Time, err error) {
	since := now.Add(-usageRateTimeConstant)
	if prev := watch.RateSampledAt; prev != nil {
		if !prev.Before(now) {
			return watch.UsageRatePerHour, *prev, nil
		}
		since = later(since, *prev)
		rate = decayRate(watch.UsageRatePerHour, now.Sub(*prev))
	}
	spent, err := q.SumUsageAmountBetween(ctx, gen.SumUsageAmountBetweenParams{
		MerchantID: watch.MerchantID, CustomerID: watch.CustomerID, Currency: watch.Currency, Since: since, Until: now,
	})
	if err != nil {
		return 0, now, err
	}
	// Spent within one time constant of an hour is that much per hour.
	return satAdd(rate, satMul(spent, int64(time.Hour/usageRateTimeConstant))), now, nil
}

// decayRate is a rate decayed exponentially over elapsed, rounded to the unit.
func decayRate(ratePerHour int64, elapsed time.Duration) int64 {
	return int64(math.Round(float64(ratePerHour) * math.Exp(-float64(elapsed)/float64(usageRateTimeConstant))))
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// WatchBalanceInTx looks at one customer's balance under its lock: it settles
// lapsed credit lots, posts each running charge through the last 00:00 UTC,
// samples the usage rate, tells the host and the customer of a low or
// depleted balance once per crossing, and writes when to look next. A
// customer with no watch is left alone; an arrears one with no running charge
// loses its watch.
func (s *MoneyService) WatchBalanceInTx(ctx context.Context, txDB *db.DB, customer identity.CustomerID, currency string) error {
	if s == nil || txDB == nil {
		return fmt.Errorf("balance watch requires a bound transaction")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	if _, err := txSvc.lockBalance(ctx, q, customer, customer.UUID().String(), currency); err != nil {
		return err
	}
	key := gen.LockBalanceWatchParams{MerchantID: merchantID.UUID(), CustomerID: customer.UUID(), Currency: currency}
	watch, err := q.LockBalanceWatch(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	now := instant(txSvc.now())
	// Lots that lapsed are settled in the order they lapsed; each posting
	// stops at every 00:00 UTC on its way.
	if _, err := txSvc.grantLedger(q, key.MerchantID).ExpireLapsedBy(ctx, key.CustomerID, currency, now); err != nil {
		return err
	}
	charges, err := q.ListRunningChargesByCustomer(ctx, gen.ListRunningChargesByCustomerParams{MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: currency})
	if err != nil {
		return err
	}
	day := now.Truncate(24 * time.Hour)
	for _, c := range charges {
		if c.PostedThroughAt.Before(day) {
			if _, err := txSvc.postRunningCharge(ctx, q, c, day, false); err != nil {
				return err
			}
		}
	}
	bal, err := txSvc.deriveBalance(ctx, q, key.MerchantID, key.CustomerID, currency)
	if err != nil {
		return err
	}
	view, err := txSvc.viewBalance(ctx, q, bal)
	if err != nil {
		return err
	}
	if !view.watched() {
		return q.DeleteBalanceWatch(ctx, gen.DeleteBalanceWatchParams{MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: currency})
	}
	usageRate, sampledAt, err := sampleUsageRate(ctx, q, watch, bal.AsOf)
	if err != nil {
		return err
	}
	cfg, err := balanceNotices(ctx, txDB)
	if err != nil {
		return err
	}
	out := view.project(resolveLowBalanceLine(cfg, view.settings, currency), usageRate)

	// Each notice is sent once per crossing. Only a funding re-arms it: a
	// released hold or a falling rate does not, so a customer hovering at
	// the line is told once.
	lowNotifiedAt, depletedAt, fundedAt := watch.LowNotifiedAt, watch.DepletedAt, watch.FundedAt
	if fundedAt != nil {
		if !out.low {
			lowNotifiedAt = nil
		}
		if !out.depleted {
			depletedAt = nil
		}
	}
	event := billing.BalanceHostEvent{
		CustomerID: billing.CustomerID(customer), Currency: currency, AvailableAmount: bal.Balance - bal.HeldBalance,
		OwedAmount: view.owed, AccrualRatePerHour: view.accrualRate, UsageRatePerHour: usageRate, LowBalanceAmount: out.line,
		AsOf: bal.AsOf, DepletesAt: out.depletesAt, RunwayEndsAt: out.runwayEndsAt,
	}
	var notice models.NotificationEventType
	if out.low && lowNotifiedAt == nil {
		if err := enqueueBalanceEvent(ctx, q, key.MerchantID, billing.HostEventBalanceLow, event); err != nil {
			return err
		}
		lowNotifiedAt, notice = &bal.AsOf, models.NotificationBalanceLow
	}
	if out.depleted && depletedAt == nil {
		if err := enqueueBalanceEvent(ctx, q, key.MerchantID, billing.HostEventBalanceDepleted, event); err != nil {
			return err
		}
		depletedAt, notice = &bal.AsOf, models.NotificationBalanceDepleted
	}
	// Low and depleted crossed in one look are one customer notice.
	if notice != "" {
		if err := notifyBalance(ctx, q, key.MerchantID, notice, event); err != nil {
			return err
		}
		fundedAt = nil
	}
	return q.SetBalanceWatch(ctx, gen.SetBalanceWatchParams{
		MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: key.Currency,
		NextCheckAt: view.next(out, lowNotifiedAt == nil, depletedAt == nil), DepletesAt: out.depletesAt,
		LowNotifiedAt: lowNotifiedAt, DepletedAt: depletedAt, FundedAt: fundedAt,
		UsageRatePerHour: usageRate, RateSampledAt: &sampledAt, Now: bal.AsOf,
	})
}

// watchBalance writes when a customer's balance is next looked at after a
// running charge starts or stops; a balance no longer watched loses its watch.
func (s *MoneyService) watchBalance(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) error {
	bal, err := s.deriveBalance(ctx, q, merchantID, customer, currency)
	if err != nil {
		return err
	}
	view, err := s.viewBalance(ctx, q, bal)
	if err != nil {
		return err
	}
	if !view.watched() {
		return q.DeleteBalanceWatch(ctx, gen.DeleteBalanceWatchParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	}
	var usageRate int64
	lowArmed, depletedArmed := true, true
	watch, err := q.GetBalanceWatch(ctx, gen.GetBalanceWatchParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	switch {
	case err == nil:
		usageRate, lowArmed, depletedArmed = watch.UsageRatePerHour, watch.LowNotifiedAt == nil, watch.DepletedAt == nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	cfg, err := balanceNotices(ctx, s.db)
	if err != nil {
		return err
	}
	out := view.project(resolveLowBalanceLine(cfg, view.settings, currency), usageRate)
	next := view.next(out, lowArmed, depletedArmed)
	// A charge started in the past may already be due a day's checkpoint, and
	// a funding may be waiting to re-arm a notice.
	if view.checkpointDue || (watch.FundedAt != nil && (!lowArmed || !depletedArmed)) {
		next = &bal.AsOf
	}
	return q.UpsertBalanceWatch(ctx, gen.UpsertBalanceWatchParams{
		MerchantID: merchantID, CustomerID: customer, Currency: currency,
		NextCheckAt: next, DepletesAt: out.depletesAt, Now: bal.AsOf,
	})
}

// LowBalanceLines are the effective low-balance lines of a customer's
// balances in currencies, as its watch judges them. It reads through s's own
// connection.
func (s *MoneyService) LowBalanceLines(ctx context.Context, customer identity.CustomerID, currencies []string) ([]billing.EffectiveLowBalanceLine, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := balanceNotices(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]billing.EffectiveLowBalanceLine, 0, len(currencies))
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListMoneyAccountSettingsByCustomer(ctx, gen.ListMoneyAccountSettingsByCustomerParams{MerchantID: mid.UUID(), CustomerID: customer.UUID()})
		if err != nil {
			return err
		}
		byCurrency := make(map[string]*gen.BillingMoneySetting, len(rows))
		for i := range rows {
			byCurrency[rows[i].Currency] = &rows[i]
		}
		for _, currency := range currencies {
			out = append(out, resolveLowBalanceLine(cfg, byCurrency[currency], currency).effective(currency))
		}
		return nil
	})
	return out, err
}

// SetLowBalanceLineTx writes a customer's own low-balance line in a currency
// (nil parts inherit the merchant's) in the caller's transaction, under the
// customer lock. The low notice is re-armed and the watch looked at on the
// next pass.
func SetLowBalanceLineTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, line billing.LowBalanceLine, now time.Time) error {
	if _, err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
		MerchantID: merchantID, CustomerID: customer, Currency: line.Currency, BillingMode: BillingModePrepaid, Now: now,
	}); err != nil {
		return err
	}
	var hours *int32
	if line.RunwayHours != nil {
		h := int32(*line.RunwayHours) // #nosec G115 -- validated to MaxLowBalanceRunwayHours
		hours = &h
	}
	if err := q.SetMoneyAccountLowBalanceLine(ctx, gen.SetMoneyAccountLowBalanceLineParams{
		MerchantID: merchantID, CustomerID: customer, Currency: line.Currency,
		LowBalanceAmount: line.Amount, LowBalanceRunwayHours: hours, Now: now,
	}); err != nil {
		return err
	}
	return q.WatchCustomerBalance(ctx, gen.WatchCustomerBalanceParams{MerchantID: merchantID, CustomerID: customer, Currency: line.Currency, RearmLow: true, Now: now})
}

// ValidateLowBalanceLine refuses a negative part or a runway over a year;
// param prefixes the refused field's name.
func ValidateLowBalanceLine(line billing.LowBalanceLine, param string) error {
	if line.Amount != nil && *line.Amount < 0 {
		return apperr.Invalidf("amount must be nonnegative").WithParam(param + "amount")
	}
	if h := line.RunwayHours; h != nil && (*h < 0 || *h > billing.MaxLowBalanceRunwayHours) {
		return apperr.Invalidf("runway_hours must be 0 to %d", billing.MaxLowBalanceRunwayHours).WithParam(param + "runway_hours")
	}
	return nil
}

// enqueueBalanceEvent writes one balance event to the host outbox, keyed by
// the instant its crossing was seen.
func enqueueBalanceEvent(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, eventType billing.HostEventType, event billing.BalanceHostEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
		MerchantID: merchantID, EventType: string(eventType), SubjectType: "customer", SubjectID: event.CustomerID.UUID(),
		Currency: event.Currency, OccurredAt: event.AsOf, Data: data,
		DedupeKey: fmt.Sprintf("%s:%s:%s:%s", eventType, event.CustomerID, event.Currency, event.AsOf.Format(runningChargeInstantLayout)),
	})
	return err
}

// notifyBalance writes the customer's notification event of a crossing, its
// id derived from the crossing so a retried look writes it once. The
// application delivers it.
func notifyBalance(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, kind models.NotificationEventType, event billing.BalanceHostEvent) error {
	available, owed, line := event.AvailableAmount, event.OwedAmount, event.LowBalanceAmount
	data, err := json.Marshal(billing.NotificationData{
		Currency: event.Currency, AvailableAmount: &available, OwedAmount: &owed, LowBalanceAmount: &line, RunwayEndsAt: event.RunwayEndsAt,
	})
	if err != nil {
		return err
	}
	customer := event.CustomerID.UUID()
	return q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
		ID:         uuid.NewSHA1(customer, []byte(string(kind)+":"+event.Currency+":"+event.AsOf.Format(runningChargeInstantLayout))),
		MerchantID: merchantID, CustomerID: customer, EventType: string(kind), Data: data, CreatedAt: event.AsOf,
	})
}
