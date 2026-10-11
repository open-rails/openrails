package service

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// GetBalance returns a customer's money in one currency.
func (s *Service) GetBalance(ctx context.Context, customer identity.CustomerID, currency string) (*billing.Balance, error) {
	currency, err := requireCurrency(currency)
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
	return out, err
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

// readUsageParams reads a usage report request. A cursor reads the rest of
// its own report: a window the request leaves out is the cursor's, and
// without a cursor it ends now and starts a month before.
func readUsageParams(params billing.GetUsageParams, now func() time.Time) (usageReport, error) {
	currency, err := requireCurrency(params.Currency)
	if err != nil {
		return usageReport{}, err
	}
	out := usageReport{currency: currency, groupBy: cmp.Or(params.GroupBy, billing.UsageByEventType), from: params.From.UTC(), to: params.To.UTC()}
	switch out.groupBy {
	case billing.UsageByEventType, billing.UsageByResource, billing.UsageByInvoker:
	default:
		return usageReport{}, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "group_by must be event_type, resource or invoker").WithParam("group_by")
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

// GetUsage reads a page of a customer's usage in one currency over [From, To),
// grouped by GroupBy (event_type when empty): by total amount, largest first,
// then by key in byte order.
func (s *Service) GetUsage(ctx context.Context, customer identity.CustomerID, params billing.GetUsageParams) (*billing.Usage, error) {
	if customer.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	report, err := readUsageParams(params, s.now)
	if err != nil {
		return nil, err
	}
	out := &billing.Usage{CustomerID: customer, Currency: report.currency, From: report.from, To: report.to, GroupBy: report.groupBy, Rows: []billing.UsageRow{}}
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		q := s.rt.DB.Gen(ctx)
		arg := gen.UsageReportParams{GroupBy: string(report.groupBy), MerchantID: mid.UUID(), CustomerID: customer.UUID(), Currency: report.currency,
			FromAt: report.from, ToAt: report.to, RowLimit: UsagePage.Fetch(report.limit)}
		if report.after != nil {
			arg.AfterAmount, arg.AfterKey = &report.after.Amount, &report.after.Key
		}
		rows, err := q.UsageReport(ctx, arg)
		if err != nil {
			return err
		}
		page := pagination.Cut(rows, report.limit, func(r gen.UsageReportRow) any {
			return usageCursor{Amount: r.TotalAmount, Key: r.Key, Currency: report.currency, GroupBy: report.groupBy, From: report.from, To: report.to}
		})
		if page.Next != "" {
			out.NextCursor = &page.Next
		}
		for _, r := range page.Items {
			out.Rows = append(out.Rows, billing.UsageRow{Key: r.Key, EventCount: r.EventCount, Amount: r.TotalAmount})
		}
		if report.groupBy != billing.UsageByEventType || len(out.Rows) == 0 {
			return nil
		}
		dimensions := make(map[string]map[string]int64, len(out.Rows))
		types := make([]string, len(out.Rows))
		for i := range out.Rows {
			out.Rows[i].Dimensions = map[string]int64{}
			dimensions[out.Rows[i].Key] = out.Rows[i].Dimensions
			types[i] = out.Rows[i].Key
		}
		dims, err := q.UsageReportDimensions(ctx, gen.UsageReportDimensionsParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(),
			Currency: report.currency, EventTypes: types, FromAt: report.from, ToAt: report.to})
		if err != nil {
			return err
		}
		for _, d := range dims {
			dimensions[d.EventType][d.Key] = d.Total
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetCreditAccountSettings upserts a payer's spend policy in one currency.
func (s *Service) SetCreditAccountSettings(ctx context.Context, payer identity.CustomerID, currency string, in money.AccountSettingsInput) error {
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	currency, err := requireCurrency(currency)
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
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	return s.moneyService().GetAccountSettingsForCustomer(ctx, payer, currency)
}
