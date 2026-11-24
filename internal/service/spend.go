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
		settings, err := s.moneyService().GetAccountSettings(ctx, customer, currency)
		if err != nil {
			return err
		}
		owed, err := s.moneyService().GetOutstandingOwed(ctx, customer, currency)
		if err != nil {
			return err
		}
		out, err = s.moneyService().PublicBalance(ctx, bal, billing.BillingMode(settings.BillingMode), owed)
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
	GroupBy  billing.UsageGroupBy `json:"g"`
	From     time.Time            `json:"f"`
	To       time.Time            `json:"t"`
}

// usageReport is a usage report request read: its currency, grouping,
// window and page, and the group the page follows (nil: the first page).
type usageReport struct {
	currency string
	groupBy  billing.UsageGroupBy
	from, to time.Time
	limit    int
	after    *usageCursor
}

// readUsageParams reads a usage report request, its currency already checked.
// A cursor reads the rest of its own report: a window the request leaves out
// is the cursor's, and without a cursor it ends now and starts a month before.
func readUsageParams(params billing.GetUsageParams, now func() time.Time) (usageReport, error) {
	var err error
	out := usageReport{currency: string(params.Currency), groupBy: cmp.Or(params.GroupBy, billing.UsageByEventType), from: params.From.UTC(), to: params.To.UTC()}
	switch out.groupBy {
	case billing.UsageByEventType, billing.UsageByInvoker, billing.UsageByMeter:
	default:
		return usageReport{}, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "group_by must be event_type, invoker or meter").WithParam("group_by")
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
		if after.Currency != out.currency || after.GroupBy != out.groupBy || after.From.IsZero() || after.To.IsZero() ||
			(!out.from.IsZero() && !out.from.Equal(after.From)) || (!out.to.IsZero() && !out.to.Equal(after.To)) {
			return usageReport{}, pagination.ErrInvalidCursor
		}
		out.from, out.to, out.after = after.From.UTC(), after.To.UTC(), &after
	}
	if out.from.IsZero() || out.to.IsZero() {
		at := now().UTC()
		if out.to.IsZero() {
			out.to = at
		}
		if out.from.IsZero() {
			out.from = at.AddDate(0, -1, 0)
		}
	}
	if !out.from.Before(out.to) {
		return usageReport{}, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "from must be before to").WithParam("from")
	}
	return out, nil
}

// GetUsage reads a page of a customer's usage in one currency on the days of
// the merchant's accounting zone overlapping [From, To), from its history in
// the event store, grouped by GroupBy (event_type when empty): by total
// amount, largest first, then by key in byte order. A day's amount is its
// buckets' share of the period's price, so the days sum to what posts.
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
		from, to, err := s.moneyService().UsageDays(ctx, report.from, report.to)
		if err != nil {
			return err
		}
		rows, err := s.moneyService().UsageHistory(ctx, eventstore.HistoryFilter{CustomerID: billing.CustomerID(customer.UUID()), Currency: report.currency, From: from, To: to})
		if err != nil {
			return err
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
			return usageCursor{Amount: g.amount, Key: g.key, Currency: report.currency, GroupBy: report.groupBy, From: report.from, To: report.to}
		})
		if page.Next != "" {
			out.NextCursor = &page.Next
		}
		for _, g := range page.Items {
			row := billing.UsageRow{Key: g.key, EventCount: g.events, Amount: g.amount}
			if report.groupBy == billing.UsageByMeter {
				meter, cell, _ := strings.Cut(g.key, "\x1f")
				quantity := g.quantity
				row.Key, row.Quantity = meter, &quantity
				if cell != "" {
					row.Cell = &cell
				}
			}
			out.Rows = append(out.Rows, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// usageGroup is one group of a usage report.
type usageGroup struct {
	key                      string
	events, amount, quantity int64
}

// usageGroups groups priced history: by event type or invoker, counting the
// events once, on their rows with no meter; by meter, what each meter and
// matrix cell (joined by U+001F) charged. Ordered by amount, largest first,
// then key in byte order.
func usageGroups(rows []money.UsageHistoryRow, by billing.UsageGroupBy) []usageGroup {
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
		g.amount += r.Amount
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
