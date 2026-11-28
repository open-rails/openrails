package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/catalog"

	"github.com/open-rails/openrails/internal/modules/money/ledger"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/balancelook"
	"github.com/open-rails/openrails/internal/billingfact"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A balance watch is the one stored instant per watched balance (every
// prepaid one, every one with a running charge and every one holding a credit
// lot that lapses): when it is next looked at. A change in what the customer
// may spend marks it due (the ledger's ApplyIdempotent, an admission's hold),
// and the watcher job looks at the due ones. Time alone moves a balance only
// through a running charge's accrual and a credit lot's lapse, and a look
// schedules both: the lapse is expired at the look its ends_at wakes.

// lowBalanceLine is the amount a balance is low at or under: the customer's
// own, else the merchant's, else none (0).
type lowBalanceLine struct {
	amount int64
	source billing.LowBalanceLineSource
}

// resolveLowBalanceLine resolves the line: the customer's own (own, nil with
// none), else the merchant's.
func resolveLowBalanceLine(cfg models.BalanceNoticeConfiguration, own *gen.BillingCustomerLowBalanceLine, currency string) lowBalanceLine {
	if own != nil {
		return lowBalanceLine{amount: own.Amount, source: billing.LowBalanceLineCustomer}
	}
	if amount, ok := cfg.LowAmounts[currency]; ok {
		return lowBalanceLine{amount: amount, source: billing.LowBalanceLineMerchant}
	}
	return lowBalanceLine{source: billing.LowBalanceLineNone}
}

// customerLine is the customer's own line in a currency; nil with none.
func customerLine(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) (*gen.BillingCustomerLowBalanceLine, error) {
	row, err := q.GetCustomerLowBalanceLine(ctx, gen.GetCustomerLowBalanceLineParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// effectiveLine is the line a balance is judged at.
func effectiveLine(ctx context.Context, d *db.DB, q *gen.Queries, merchantID, customer uuid.UUID, currency string) (lowBalanceLine, error) {
	cfg, err := balanceNotices(ctx, d)
	if err != nil {
		return lowBalanceLine{}, err
	}
	own, err := customerLine(ctx, q, merchantID, customer, currency)
	if err != nil {
		return lowBalanceLine{}, err
	}
	return resolveLowBalanceLine(cfg, own, currency), nil
}

func (l lowBalanceLine) effective(currency string) billing.EffectiveLowBalanceLine {
	return billing.EffectiveLowBalanceLine{Currency: catalog.Currency(currency), Amount: l.amount, Source: l.source}
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
	// available is the balance less pending usage and accrual.
	available int64
	// lapsed: a lot lapsed and its expiry has not posted. Its units are in
	// the balance until it does, which the look does at once.
	lapsed bool
	lots   []creditLot
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

// runningCharges are the customer's running charges in bal's currency.
func runningCharges(ctx context.Context, q *gen.Queries, bal *models.MoneyBalance) ([]gen.BillingRunningCharge, error) {
	return q.ListRunningChargesByCustomer(ctx, gen.ListRunningChargesByCustomerParams{
		MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency,
	})
}

// viewBalance reads what projecting bal needs, given the customer's running
// charges in its currency.
func (s *MoneyService) viewBalance(ctx context.Context, q *gen.Queries, bal *models.MoneyBalance, charges []gen.BillingRunningCharge) (balanceView, error) {
	v := balanceView{asOf: bal.AsOf, charges: charges}
	var err error
	lines := runningLines(charges)
	v.accrualRate = lines.ratePerHour
	if v.owed, err = s.moneyLedger(q, bal.MerchantID).OutstandingOwed(ctx, bal.CustomerID, bal.Currency); err != nil {
		return v, err
	}
	settings, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency})
	switch {
	case err == nil:
		capacity, err := s.usageAdmissionCapacity(ctx, q, identity.CustomerID(bal.CustomerID), bal.Currency, AdmissionCapacity{BillingMode: settings.BillingMode, CreditLimit: settings.CreditLimitAmount, OutstandingOwed: v.owed})
		if err != nil {
			return v, err
		}
		settings.BillingMode, settings.CreditLimitAmount = capacity.BillingMode, capacity.CreditLimit
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
		v.lots[i] = creditLot{remaining: row.Available, endsAt: row.ExpiresAt}
	}
	lapsed, err := q.ListLapsedCreditLots(ctx, gen.ListLapsedCreditLotsParams{
		MerchantID: bal.MerchantID, CustomerID: bal.CustomerID, Currency: bal.Currency, AsOf: bal.AsOf,
	})
	if err != nil {
		return v, err
	}
	v.lapsed, v.available = len(lapsed) > 0, bal.Available()
	return v, nil
}

// balanceProjection is what a balance does next.
type balanceProjection struct {
	// depletesAt is when capacity reaches zero at the running charges' rate
	// and the lots' lapses, exact; nil when neither moves it there.
	depletesAt *time.Time
	depleted   bool
	// line is the low line; 0 is none. lowAt is the exact instant the
	// balance reaches it, nil when nothing exact moves it there.
	line  int64
	lowAt *time.Time
	low   bool
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

func (v balanceView) project(line lowBalanceLine) balanceProjection {
	var p balanceProjection
	p.depletesAt = v.exactAt(v.floor)
	p.depleted = p.depletesAt != nil && !p.depletesAt.After(v.asOf)
	if p.line = line.amount; p.line > 0 {
		p.lowAt = v.exactAt(satAdd(v.spendFloor, p.line))
		p.low = p.lowAt != nil && !p.lowAt.After(v.asOf)
	}
	return p
}

// next is when to look again: now while a lapsed lot is not expired, the
// next lot's lapse, and each armed crossing an exact cause (a running
// charge's accrual, a lapse) moves the balance to. Nil: only a mark.
func (v balanceView) next(p balanceProjection, lowArmed, depletedArmed bool) *time.Time {
	var next *time.Time
	consider := func(at *time.Time) {
		if at != nil && (next == nil || at.Before(*next)) {
			t := *at
			next = &t
		}
	}
	if v.lapsed {
		consider(&v.asOf)
	}
	for _, lot := range v.lots {
		if lot.remaining > 0 {
			consider(lot.endsAt)
		}
	}
	if lowArmed {
		consider(p.lowAt)
	}
	if depletedArmed {
		consider(p.depletesAt)
	}
	return next
}

// watched reports whether the balance keeps a watch: every prepaid one, every
// one with a running charge and every one holding a lot that lapses.
func (v balanceView) watched() bool {
	if !v.arrears || len(v.charges) > 0 || v.lapsed {
		return true
	}
	for _, lot := range v.lots {
		if lot.remaining > 0 && lot.endsAt != nil {
			return true
		}
	}
	return false
}

// WatchBalanceInTx looks at one customer's balance under its lock: it expires
// lapsed credit lots, tells the host and the customer of a low or depleted
// balance once per crossing, and writes when to look next. A customer with no
// watch, or one not due, is left alone; an arrears one with no running charge
// and no lot that lapses loses its watch.
func (s *MoneyService) WatchBalanceInTx(ctx context.Context, txDB *db.DB, customer identity.CustomerID, currency string) error {
	if s == nil || txDB == nil {
		return fmt.Errorf("balance watch requires a bound transaction")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	txSvc := s.WithDatabase(txDB)
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
	// A look that ran while this one waited for the lock moved the watch on.
	if watch.NextCheckAt == nil || watch.NextCheckAt.After(now) {
		return nil
	}
	// Lots that lapsed are settled in the order they lapsed.
	expired, err := txSvc.creditLots(q, key.MerchantID).ExpireLapsedBy(ctx, key.CustomerID, currency, now)
	if err != nil {
		return err
	}
	bal, err := txSvc.deriveBalance(ctx, q, key.MerchantID, key.CustomerID, currency)
	if err != nil {
		return err
	}
	charges, err := runningCharges(ctx, q, bal)
	if err != nil {
		return err
	}
	view, err := txSvc.viewBalance(ctx, q, bal, charges)
	if err != nil {
		return err
	}
	if !view.watched() {
		return q.DeleteBalanceWatch(ctx, gen.DeleteBalanceWatchParams{MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: currency})
	}
	line, err := effectiveLine(ctx, txDB, q, key.MerchantID, key.CustomerID, currency)
	if err != nil {
		return err
	}
	out := view.project(line)

	// Each notice is sent once per crossing. Only a funding re-arms it: a
	// released hold does not, so a customer hovering at the line is told
	// once.
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
		CustomerID: billing.CustomerID(customer), Currency: catalog.Currency(currency), AvailableAmount: bal.Available(),
		OwedAmount: view.owed, AccrualRatePerHour: view.accrualRate, LowBalanceAmount: out.line,
		AsOf: bal.AsOf, DepletesAt: out.depletesAt,
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
		reason := ""
		if notice == models.NotificationBalanceDepleted && expired > 0 {
			reason = "expired"
		}
		if err := notifyBalance(ctx, q, key.MerchantID, notice, event, reason); err != nil {
			return err
		}
		fundedAt = nil
	}
	next := view.next(out, lowNotifiedAt == nil, depletedAt == nil)
	low, floor := view.lines(out)
	if err := q.SetBalanceWatch(ctx, gen.SetBalanceWatchParams{
		MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: key.Currency,
		NextCheckAt: next, DepletesAt: out.depletesAt,
		LowNotifiedAt: lowNotifiedAt, DepletedAt: depletedAt, FundedAt: fundedAt, Now: bal.AsOf,
		LowLineAmount: low, FloorAmount: floor, LookEachChange: view.moving(),
	}); err != nil {
		return err
	}
	return scheduleLook(ctx, q, key.MerchantID, key.CustomerID, currency, next)
}

// lines are what a change is judged against until the next look: the
// available amount at or under which the balance is low (nil with no line),
// and the one at which it is depleted.
func (v balanceView) lines(p balanceProjection) (low, floor *int64) {
	if p.line > 0 {
		at := satAdd(v.spendFloor, p.line)
		low = &at
	}
	return low, &v.floor
}

// moving reports a balance time moves: a running charge accrues, or a lot
// lapses. A change moves its run-out, so each is looked at.
func (v balanceView) moving() bool {
	if v.accrualRate > 0 || v.lapsed {
		return true
	}
	for _, lot := range v.lots {
		if lot.remaining > 0 && lot.endsAt != nil {
			return true
		}
	}
	return false
}

// scheduleLook queues the look for the instant a look said to look again, in
// q's transaction. Without a River producer the billing pass looks at it.
func scheduleLook(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, at *time.Time) error {
	if at == nil {
		return nil
	}
	when := at.UTC()
	args := balancelook.Args{MerchantID: merchant, CustomerID: customer, Currency: currency, At: &when}
	opts := args.InsertOpts()
	opts.ScheduledAt = when
	_, err := db.InsertRiverJobWith(ctx, q, args, &opts)
	if errors.Is(err, db.ErrNoRiverProducer) {
		return nil
	}
	return err
}

// watchBalance writes when a customer's balance is next looked at after a
// running charge starts or stops; a balance no longer watched loses its watch.
func (s *MoneyService) watchBalance(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) error {
	bal, err := s.deriveBalance(ctx, q, merchantID, customer, currency)
	if err != nil {
		return err
	}
	charges, err := runningCharges(ctx, q, bal)
	if err != nil {
		return err
	}
	view, err := s.viewBalance(ctx, q, bal, charges)
	if err != nil {
		return err
	}
	if !view.watched() {
		return q.DeleteBalanceWatch(ctx, gen.DeleteBalanceWatchParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	}
	lowArmed, depletedArmed := true, true
	watch, err := q.GetBalanceWatch(ctx, gen.GetBalanceWatchParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	switch {
	case err == nil:
		lowArmed, depletedArmed = watch.LowNotifiedAt == nil, watch.DepletedAt == nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	line, err := effectiveLine(ctx, s.db, q, merchantID, customer, currency)
	if err != nil {
		return err
	}
	out := view.project(line)
	next := view.next(out, lowArmed, depletedArmed)
	// A funding may be waiting to re-arm a notice.
	if watch.FundedAt != nil && (!lowArmed || !depletedArmed) {
		next = &bal.AsOf
	}
	low, floor := view.lines(out)
	if err := q.UpsertBalanceWatch(ctx, gen.UpsertBalanceWatchParams{
		MerchantID: merchantID, CustomerID: customer, Currency: currency,
		NextCheckAt: next, DepletesAt: out.depletesAt, Now: bal.AsOf,
		LowLineAmount: low, FloorAmount: floor, LookEachChange: view.moving(),
	}); err != nil {
		return err
	}
	return scheduleLook(ctx, q, merchantID, customer, currency, next)
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
		rows, err := s.db.Gen(ctx).ListCustomerLowBalanceLines(ctx, gen.ListCustomerLowBalanceLinesParams{
			MerchantID: mid.UUID(), CustomerIds: []uuid.UUID{customer.UUID()}, RowLimit: int32(len(catalog.Currencies())), // #nosec G115 -- the currency registry
		})
		if err != nil {
			return err
		}
		byCurrency := make(map[string]*gen.BillingCustomerLowBalanceLine, len(rows))
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
// (a nil amount inherits the merchant's) in the caller's transaction, under
// the customer lock. It decides no billing mode. A changed line re-arms the
// low notice and the watch is looked at on the next pass; the same line
// again writes nothing.
func SetLowBalanceLineTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, line billing.LowBalanceLine, now time.Time) error {
	current, err := customerLine(ctx, q, merchantID, customer, string(line.Currency))
	if err != nil {
		return err
	}
	var curAmount *int64
	if current != nil {
		curAmount = &current.Amount
	}
	if equalPtr(curAmount, line.Amount) {
		return nil
	}
	if line.Amount == nil {
		err = q.DeleteCustomerLowBalanceLine(ctx, gen.DeleteCustomerLowBalanceLineParams{MerchantID: merchantID, CustomerID: customer, Currency: string(line.Currency)})
	} else {
		err = q.UpsertCustomerLowBalanceLine(ctx, gen.UpsertCustomerLowBalanceLineParams{
			MerchantID: merchantID, CustomerID: customer, Currency: string(line.Currency), Amount: *line.Amount, Now: now,
		})
	}
	if err != nil {
		return err
	}
	return ledger.WatchCustomerBalance(ctx, q, gen.WatchCustomerBalanceParams{MerchantID: merchantID, CustomerID: customer, Currency: string(line.Currency), RearmLow: true, Now: now})
}

func equalPtr[T comparable](a, b *T) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// ValidateLowBalanceLine refuses a negative amount; param prefixes the
// refused field's name.
func ValidateLowBalanceLine(line billing.LowBalanceLine, param string) error {
	if line.Amount != nil && *line.Amount < 0 {
		return apperr.Invalidf("amount must be nonnegative").WithParam(param + "amount")
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
		Currency: string(event.Currency), OccurredAt: event.AsOf, Data: data,
		DedupeKey: fmt.Sprintf("%s:%s:%s:%s", eventType, event.CustomerID, event.Currency, event.AsOf.Format(instantLayout)),
	})
	return err
}

// notifyBalance writes the customer's notification event of a crossing, its
// id derived from the crossing so a retried look writes it once. It is
// emailed when the merchant turns its type on.
func notifyBalance(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, kind models.NotificationEventType, event billing.BalanceHostEvent, reason string) error {
	available, owed, line := event.AvailableAmount, event.OwedAmount, event.LowBalanceAmount
	data, err := json.Marshal(billing.NotificationData{
		Currency: event.Currency, AvailableAmount: &available, OwedAmount: &owed, LowBalanceAmount: &line, Reason: reason,
	})
	if err != nil {
		return err
	}
	customer := event.CustomerID.UUID()
	_, err = billingfact.Record(ctx, q, billingfact.Fact{
		ID:         uuidutil.DerivedV7(event.AsOf, customer, string(kind)+":"+string(event.Currency)+":"+event.AsOf.Format(instantLayout)),
		MerchantID: merchantID, CustomerID: customer, EventType: string(kind), Data: data, CreatedAt: event.AsOf,
	})
	return err
}
