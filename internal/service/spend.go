package service

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// GetBalance returns a customer's money in one currency.
func (s *Service) GetBalance(ctx context.Context, customer identity.CustomerID, currency string) (*billing.Balance, error) {
	currency, err := s.balanceCurrency(ctx, "currency", currency)
	if err != nil {
		return nil, err
	}
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	var out *billing.Balance
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		bal, err := s.moneyService().GetBalanceForCustomer(ctx, customer, currency)
		if err != nil {
			return err
		}
		billingMode, err := s.moneyService().GetAdmissionBillingMode(ctx, customer, currency)
		if err != nil {
			return err
		}
		owed, err := s.moneyService().GetOutstandingOwed(ctx, customer, currency)
		if err != nil {
			return err
		}
		out, err = s.moneyService().PublicBalance(ctx, bal, billing.BillingMode(billingMode), owed)
		return err
	})
	if err != nil {
		return nil, err
	}
	one := []billing.Balance{*out}
	if err := s.withUnits(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

// UsagePage is a usage report's page size.
var UsagePage = pagination.Size{Default: billing.DefaultUsagePageLimit, Max: billing.MaxUsagePageLimit}

// usageCursor is where a page of a usage report ends: its last group, and the
// report it belongs to.
type usageCursor struct {
	Amount   int64                `json:"a"`
	Key      string               `json:"k"`
	Currency string               `json:"c"`
	Zone     string               `json:"z"`
	GroupBy  billing.UsageGroupBy `json:"g"`
	From     time.Time            `json:"f"`
	To       time.Time            `json:"t"`
}

// usageReport is a usage report request read: its currency, zone (nil: the
// merchant's accounting zone), grouping, window and page, and the group the
// page follows (nil: the first page).
type usageReport struct {
	currency string
	zone     *time.Location
	groupBy  billing.UsageGroupBy
	from, to time.Time
	limit    int
	after    *usageCursor
}

// usageGrouping reads a usage read's group_by: event_type when empty.
func usageGrouping(by billing.UsageGroupBy) (billing.UsageGroupBy, error) {
	switch by = cmp.Or(by, billing.UsageByEventType); by {
	case billing.UsageByEventType, billing.UsageByInvoker, billing.UsageByMeter:
		return by, nil
	}
	return "", apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "group_by must be event_type, invoker or meter").WithParam("group_by")
}

// usageZone reads a usage read's timezone: an IANA zone, or nil when empty
// for the merchant's accounting zone.
func usageZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	zone, err := time.LoadLocation(name)
	if name == "Local" || err != nil {
		return nil, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, fmt.Sprintf("timezone %q is not an IANA time zone name", name)).WithParam("timezone")
	}
	return zone, nil
}

// usageWindow is [from, to), the month before now where either is zero.
func usageWindow(from, to time.Time, now func() time.Time) (time.Time, time.Time, error) {
	from, to = from.UTC(), to.UTC()
	if from.IsZero() || to.IsZero() {
		at := now().UTC()
		if to.IsZero() {
			to = at
		}
		if from.IsZero() {
			from = at.AddDate(0, -1, 0)
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "from must be before to").WithParam("from")
	}
	return from, to, nil
}

// readUsageParams reads a usage report request, its currency already checked.
// A cursor reads the rest of its own report: a window the request leaves out
// is the cursor's, and without a cursor it ends now and starts a month before.
func readUsageParams(params billing.GetUsageParams, now func() time.Time) (usageReport, error) {
	var err error
	out := usageReport{currency: string(params.Currency), from: params.From.UTC(), to: params.To.UTC()}
	if out.groupBy, err = usageGrouping(params.GroupBy); err != nil {
		return usageReport{}, err
	}
	if out.zone, err = usageZone(params.Timezone); err != nil {
		return usageReport{}, err
	}
	if out.limit, err = UsagePage.Limit(params.PageRequest); err != nil {
		return usageReport{}, err
	}
	var after usageCursor
	paged, err := pagination.Decode(params.Cursor, &after)
	if err != nil {
		return usageReport{}, err
	}
	if paged {
		if after.Currency != out.currency || after.Zone != params.Timezone || after.GroupBy != out.groupBy || after.From.IsZero() || after.To.IsZero() ||
			(!out.from.IsZero() && !out.from.Equal(after.From)) || (!out.to.IsZero() && !out.to.Equal(after.To)) {
			return usageReport{}, pagination.ErrInvalidCursor
		}
		out.from, out.to, out.after = after.From.UTC(), after.To.UTC(), &after
	}
	if out.from, out.to, err = usageWindow(out.from, out.to, now); err != nil {
		return usageReport{}, err
	}
	return out, nil
}

// GetUsage reads a page of a customer's usage in one currency on the days of
// params.Timezone (the merchant's accounting zone when empty) overlapping
// [From, To), grouped by GroupBy (event_type when empty): by total amount,
// largest first, then by key in byte order. Amounts are the prices intake
// took; a volume tier's fall is the report's Discount, so the rows less it
// sum to what posts. By event type or meter it reads the history in the
// event store; by invoker the raw events, kept eventstore.EventsKept, and a
// window that starts before them is refused.
func (s *Service) GetUsage(ctx context.Context, customer identity.CustomerID, params billing.GetUsageParams) (*billing.Usage, error) {
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := s.balanceCurrency(ctx, "currency", string(params.Currency))
	if err != nil {
		return nil, err
	}
	params.Currency = catalog.Currency(currency)
	report, err := readUsageParams(params, s.now)
	if err != nil {
		return nil, err
	}
	if err := s.rt.RequireEventStore("usage history"); err != nil {
		return nil, err
	}
	out := &billing.Usage{CustomerID: customer, Currency: catalog.Currency(report.currency), From: report.from, To: report.to, GroupBy: report.groupBy, Rows: []billing.UsageRow{}}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		zone := report.zone
		if zone == nil {
			if zone, err = s.moneyService().AccountingZone(ctx); err != nil {
				return err
			}
		}
		out.Timezone = zone.String()
		from, to := money.LocalDays(zone, report.from, report.to)
		rows, err := s.usageRows(ctx, report.groupBy, eventstore.HistoryFilter{CustomerID: billing.CustomerID(customer.UUID()), Currency: report.currency, From: from, To: to, Zone: zone})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out.Discount += r.Discount
		}
		groups := usageGroups(rows, report.groupBy)
		if report.after != nil {
			groups = slices.DeleteFunc(groups, func(g usageGroup) bool {
				return g.amount > report.after.Amount || g.amount == report.after.Amount && g.key <= report.after.Key
			})
		}
		if n := int(UsagePage.Fetch(report.limit)); len(groups) > n {
			groups = groups[:n]
		}
		page := pagination.Cut(groups, report.limit, func(g usageGroup) any {
			return usageCursor{Amount: g.amount, Key: g.key, Currency: report.currency, Zone: params.Timezone, GroupBy: report.groupBy, From: report.from, To: report.to}
		})
		if page.Next != "" {
			out.NextCursor = &page.Next
		}
		out.Rows = groupRows(page.Items, report.groupBy)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetUsageSeries reads a customer's usage in one currency over time: one
// bucket per params.Interval on params.Timezone's calendar (the merchant's
// accounting zone when empty) overlapping [From, To), empty ones included,
// each with its groups ordered as GetUsage orders them. Buckets start at
// their days' first instants (calendarutil.Buckets): an hour series has 23
// or 25 buckets on a day the clocks change, and weeks start Monday. Hours are
// read from quarter hours, longer buckets from the zone's days, each read as
// GetUsage reads it (by invoker from raw events). More than
// billing.MaxUsageBuckets buckets is refused.
func (s *Service) GetUsageSeries(ctx context.Context, customer identity.CustomerID, params billing.GetUsageSeriesParams) (*billing.UsageSeries, error) {
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	currency, err := s.balanceCurrency(ctx, "currency", string(params.Currency))
	if err != nil {
		return nil, err
	}
	switch params.Interval {
	case billing.UsageByHour, billing.UsageByDay, billing.UsageByWeek, billing.UsageByMonth:
	default:
		return nil, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "interval must be hour, day, week or month").WithParam("interval")
	}
	groupBy, err := usageGrouping(params.GroupBy)
	if err != nil {
		return nil, err
	}
	zone, err := usageZone(params.Timezone)
	if err != nil {
		return nil, err
	}
	from, to, err := usageWindow(params.From, params.To, s.now)
	if err != nil {
		return nil, err
	}
	if err := s.rt.RequireEventStore("usage history"); err != nil {
		return nil, err
	}
	out := &billing.UsageSeries{CustomerID: customer, Currency: catalog.Currency(currency), Interval: params.Interval, GroupBy: groupBy, Buckets: []billing.UsageBucket{}}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if zone == nil {
			if zone, err = s.moneyService().AccountingZone(ctx); err != nil {
				return err
			}
		}
		grain := string(params.Interval)
		starts, end := calendarutil.Buckets(from, to, grain, zone, billing.MaxUsageBuckets)
		if len(starts) > billing.MaxUsageBuckets {
			return apperr.New(http.StatusBadRequest, billing.CodeTooManyBuckets,
				fmt.Sprintf("from %s to %s by %s is more than %d buckets: shorten the window or widen the interval", from.Format(time.RFC3339), to.Format(time.RFC3339), grain, billing.MaxUsageBuckets)).WithParam("interval")
		}
		out.Timezone, out.From, out.To = zone.String(), starts[0], end
		f := eventstore.HistoryFilter{CustomerID: billing.CustomerID(customer.UUID()), Currency: currency, From: starts[0], To: end, Zone: zone}
		if params.Interval == billing.UsageByHour {
			f.Zone = nil
		}
		rows, err := s.usageRows(ctx, groupBy, f)
		if err != nil {
			return err
		}
		index := make(map[time.Time]int, len(starts))
		for i, start := range starts {
			index[start] = i
		}
		in := make([][]eventstore.UsageBucket, len(starts))
		for _, r := range rows {
			if i, ok := index[calendarutil.BucketStart(r.BucketAt, grain, zone)]; ok {
				in[i] = append(in[i], r)
			}
		}
		for i, start := range starts {
			b := billing.UsageBucket{StartsAt: start, Rows: groupRows(usageGroups(in[i], groupBy), groupBy)}
			for _, r := range in[i] {
				b.Discount += r.Discount
			}
			out.Buckets = append(out.Buckets, b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// usageRows reads what a usage report or series groups: the history in the
// event store, or by invoker the raw events, whose window must start after
// the oldest kept (no clamp: a total that silently lost its start would
// mislead). f.Zone nil reads quarter hours.
func (s *Service) usageRows(ctx context.Context, by billing.UsageGroupBy, f eventstore.HistoryFilter) ([]eventstore.UsageBucket, error) {
	if by != billing.UsageByInvoker {
		if f.Zone == nil {
			return s.moneyService().UsageBuckets(ctx, f)
		}
		return s.moneyService().UsageHistory(ctx, f)
	}
	if kept := s.now().UTC().Add(-eventstore.EventsKept); f.From.Before(kept) {
		return nil, apperr.New(http.StatusBadRequest, billing.CodeUsageEventsExpired,
			fmt.Sprintf("usage by invoker reads raw usage events, kept %d days: from must be %s or later; group by event_type or meter to read further back",
				int(eventstore.EventsKept.Hours()/24), kept.Format(time.RFC3339))).WithParam("from")
	}
	return s.moneyService().UsageEvents(ctx, f)
}

// groupRows are groups as a usage read answers them.
func groupRows(groups []usageGroup, by billing.UsageGroupBy) []billing.UsageRow {
	out := make([]billing.UsageRow, 0, len(groups))
	for _, g := range groups {
		row := billing.UsageRow{Key: g.key, EventCount: g.events, Amount: g.amount}
		if by == billing.UsageByMeter {
			meter, cell, _ := strings.Cut(g.key, "\x1f")
			quantity := g.quantity
			row.Key, row.Quantity = meter, &quantity
			if cell != "" {
				row.Cell = &cell
			}
		}
		out = append(out, row)
	}
	return out
}

// usageGroup is one group of a usage report.
type usageGroup struct {
	key                      string
	events, amount, quantity int64
}

// usageGroups groups usage rows: by event type or invoker, counting the
// events once, on their rows with no meter; by meter, what each meter and
// matrix cell (joined by U+001F) charged. An amount is the charges at
// intake's price and what failed usage charged, before any volume discount.
// Ordered by amount, largest first, then key in byte order.
func usageGroups(rows []eventstore.UsageBucket, by billing.UsageGroupBy) []usageGroup {
	sums := map[string]*usageGroup{}
	for _, r := range rows {
		var key string
		switch {
		case by == billing.UsageByMeter && r.Meter == "":
			continue
		case by == billing.UsageByMeter:
			key = r.Meter + "\x1f" + r.Cell
		case by == billing.UsageByInvoker:
			key = r.Invoker
		default:
			key = r.EventType
		}
		g := sums[key]
		if g == nil {
			g = &usageGroup{key: key}
			sums[key] = g
		}
		g.amount += r.Amount + r.FailedAmount
		switch {
		case by == billing.UsageByMeter:
			g.events += r.Requests
			g.quantity += r.Quantity
		case r.Meter == "":
			g.events += r.Requests
		}
	}
	out := make([]usageGroup, 0, len(sums))
	for _, g := range sums {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b usageGroup) int {
		if a.amount != b.amount {
			return cmp.Compare(b.amount, a.amount)
		}
		return strings.Compare(a.key, b.key)
	})
	return out
}

// SetCreditAccountSettings upserts a payer's spend policy in one currency.
func (s *Service) SetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string, in money.AccountSettingsInput) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	currency, err := s.registryCurrency(ctx, "currency", currency)
	if err != nil {
		return err
	}
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.moneyService().UpsertAccountSettings(ctx, payer, currency, in)
		return err
	})
}

// GetCreditAccountSettings returns a payer's stored account settings in one
// currency (billing mode, default card, credit limit).
func (s *Service) GetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	currency, err := s.registryCurrency(ctx, "currency", currency)
	if err != nil {
		return nil, err
	}
	return s.moneyService().GetAccountSettingsForCustomer(ctx, payer, currency)
}
