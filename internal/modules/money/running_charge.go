package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A running charge is (customer, currency, rate per hour, started_at) until it
// is stopped. Nothing is written while it runs: what it has accrued is part of
// the held total, and reaches the ledger in a posting (running_charge_posting.go).
// The customer row is the money lock: start, stop and every posting hold it.

const (
	// runningChargeClockSkew is how far ahead of the clock a host's instant
	// may be: the host and OpenRails keep two clocks.
	runningChargeClockSkew    = 60 * time.Second
	runningChargeMaxTextBytes = 255
)

// ErrRunningChargeNotFound: no charge was started under the reference.
var ErrRunningChargeNotFound = apperr.New(http.StatusNotFound, "running_charge_not_found", "no running charge was started under this reference")

func runningChargeInvalid(param, format string, args ...any) error {
	return apperr.Invalidf(format, args...).WithParam(param)
}

// runningChargeTermsChanged refuses a start or stop that repeats a reference
// with another term.
func runningChargeTermsChanged(reference, field string, committed, retried any) error {
	return apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused,
		fmt.Sprintf("running charge %q repeated with %s=%v, committed with %v", reference, field, retried, committed)).WithParam(field)
}

// instant is a host or clock instant as PostgreSQL stores it.
func instant(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// validateRunningChargeStart refuses malformed terms and answers the start
// instant. A reference is exact text that survives JSON, a URL path segment
// and PostgreSQL unchanged.
func validateRunningChargeStart(in billing.StartRunningChargeParams, now time.Time) (time.Time, error) {
	switch {
	case strings.TrimSpace(in.Reference) == "" || strings.TrimSpace(in.Reference) != in.Reference || in.Reference == "." || in.Reference == "..":
		return time.Time{}, runningChargeInvalid("reference", "reference required in canonical form")
	case len(in.Reference) > runningChargeMaxTextBytes || !utf8.ValidString(in.Reference) || strings.ContainsRune(in.Reference, 0):
		return time.Time{}, runningChargeInvalid("reference", "reference must be at most %d bytes of UTF-8", runningChargeMaxTextBytes)
	case in.CustomerID.IsZero():
		return time.Time{}, runningChargeInvalid("customer_id", "customer_id required")
	case in.RatePerHour <= 0 || in.RatePerHour > billing.MaxRunningChargeRatePerHour:
		return time.Time{}, runningChargeInvalid("rate_per_hour", "rate_per_hour must be 1 to %d", int64(billing.MaxRunningChargeRatePerHour))
	case in.MinimumAmount < 0:
		return time.Time{}, runningChargeInvalid("minimum_amount", "minimum_amount must be nonnegative")
	case in.OverdraftAmount < 0:
		return time.Time{}, runningChargeInvalid("overdraft_amount", "overdraft_amount must be nonnegative")
	case len(in.Resource) > runningChargeMaxTextBytes || !utf8.ValidString(in.Resource) || strings.ContainsRune(in.Resource, 0):
		return time.Time{}, runningChargeInvalid("resource", "resource must be at most %d bytes of UTF-8", runningChargeMaxTextBytes)
	}
	if in.StartedAt == nil {
		return now, nil
	}
	startedAt := instant(*in.StartedAt)
	if startedAt.After(now.Add(runningChargeClockSkew)) || startedAt.Before(now.Add(-retention.UsageIngestWindow)) {
		return time.Time{}, runningChargeInvalid("started_at", "started_at must be within the last %d days and at most %d seconds ahead",
			int(retention.UsageIngestWindow/retention.Day), int(runningChargeClockSkew/time.Second))
	}
	return startedAt, nil
}

// StartRunningChargeInTx starts a charge, or answers the one its reference
// names, through the transaction-scoped DB db.BindMerchantTx returned. started
// is false for a charge that already existed. It never commits or rolls back
// the caller's transaction; a refusal writes nothing.
func (s *MoneyService) StartRunningChargeInTx(ctx context.Context, txDB *db.DB, in billing.StartRunningChargeParams) (charge *billing.RunningCharge, started bool, err error) {
	if s == nil || txDB == nil {
		return nil, false, fmt.Errorf("running charge requires a bound transaction")
	}
	in.Currency = normalizeCurrency(in.Currency)
	if err := moneyutil.ValidateCurrency(in.Currency); err != nil {
		return nil, false, apperr.New(http.StatusBadRequest, "currency_unsupported", err.Error()).WithParam("currency")
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	now := instant(txSvc.now())
	startedAt, err := validateRunningChargeStart(in, now)
	if err != nil {
		return nil, false, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	q := txDB.Gen(ctx)
	customer := identity.CustomerID(in.CustomerID)
	bal, err := txSvc.lockBalance(ctx, q, customer, customer.UUID().String(), in.Currency)
	if err != nil {
		return nil, false, err
	}
	byReference := gen.GetRunningChargeByReferenceParams{MerchantID: merchantID.UUID(), Reference: in.Reference}
	existing, err := q.GetRunningChargeByReference(ctx, byReference)
	if err == nil {
		return replayRunningChargeStart(existing, in, now)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	running, err := q.ListRunningChargesByCustomer(ctx, gen.ListRunningChargesByCustomerParams{
		MerchantID: merchantID.UUID(), CustomerID: customer.UUID(), Currency: in.Currency,
	})
	if err != nil {
		return nil, false, err
	}
	if in.MinimumAmount > 0 {
		lines := runningLines(running)
		lines.overdraft = min(lines.overdraft, in.OverdraftAmount)
		if len(running) == 0 {
			lines.overdraft = in.OverdraftAmount
		}
		floor, _, err := txSvc.capacityFloor(ctx, q, merchantID.UUID(), customer.UUID(), in.Currency, lines.overdraft)
		if err != nil {
			return nil, false, err
		}
		if satSub(satSub(bal.Balance, bal.HeldBalance), floor) < in.MinimumAmount {
			return nil, false, ErrInsufficientCredits
		}
	}
	// A customer never funded has no balance account yet; postings draw it.
	if _, err := ledger.New(q, merchantID.UUID()).EnsureCustomerBalance(ctx, customer.UUID(), in.Currency); err != nil {
		return nil, false, err
	}
	var resource *string
	if in.Resource != "" {
		resource = &in.Resource
	}
	row, err := q.InsertRunningCharge(ctx, gen.InsertRunningChargeParams{
		MerchantID: merchantID.UUID(), Reference: in.Reference, CustomerID: customer.UUID(), Currency: in.Currency,
		RatePerHour: in.RatePerHour, OverdraftAmount: in.OverdraftAmount,
		Resource: resource, StartedAt: startedAt, Now: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Another customer's start took the reference under its own lock.
		if existing, err = q.GetRunningChargeByReference(ctx, byReference); err != nil {
			return nil, false, err
		}
		return replayRunningChargeStart(existing, in, now)
	}
	if err != nil {
		return nil, false, err
	}
	if err := txSvc.watchBalance(ctx, q, merchantID.UUID(), customer.UUID(), in.Currency); err != nil {
		return nil, false, err
	}
	charge, err = runningChargeFromRow(row, now)
	return charge, true, err
}

// replayRunningChargeStart answers a repeated start with the charge as it
// stands, or refuses a changed term. The minimum is a gate, not a term.
func replayRunningChargeStart(row gen.BillingRunningCharge, in billing.StartRunningChargeParams, now time.Time) (*billing.RunningCharge, bool, error) {
	resource := ""
	if row.Resource != nil {
		resource = *row.Resource
	}
	checks := []struct {
		field              string
		same               bool
		committed, retried any
	}{
		{"customer_id", row.CustomerID == in.CustomerID.UUID(), row.CustomerID, in.CustomerID},
		{"currency", row.Currency == in.Currency, row.Currency, in.Currency},
		{"rate_per_hour", row.RatePerHour == in.RatePerHour, row.RatePerHour, in.RatePerHour},
		{"overdraft_amount", row.OverdraftAmount == in.OverdraftAmount, row.OverdraftAmount, in.OverdraftAmount},
		{"resource", resource == in.Resource, resource, in.Resource},
		{"started_at", in.StartedAt == nil || row.StartedAt.Equal(instant(*in.StartedAt)), row.StartedAt, in.StartedAt},
	}
	for _, c := range checks {
		if !c.same {
			return nil, false, runningChargeTermsChanged(in.Reference, c.field, c.committed, c.retried)
		}
	}
	charge, err := runningChargeFromRow(row, now)
	return charge, false, err
}

// StopRunningChargeInTx stops a charge at the host's instant and posts the
// rest of it, in a caller-owned transaction. Stopping a stopped charge at the
// same instant answers it.
func (s *MoneyService) StopRunningChargeInTx(ctx context.Context, txDB *db.DB, reference string, in billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	if s == nil || txDB == nil {
		return nil, fmt.Errorf("running charge requires a bound transaction")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	byReference := gen.GetRunningChargeByReferenceParams{MerchantID: merchantID.UUID(), Reference: reference}
	row, err := q.GetRunningChargeByReference(ctx, byReference)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunningChargeNotFound
	}
	if err != nil {
		return nil, err
	}
	customer := identity.CustomerID(row.CustomerID)
	if _, err := txSvc.lockBalance(ctx, q, customer, customer.UUID().String(), row.Currency); err != nil {
		return nil, err
	}
	// State is read again under the customer lock.
	if row, err = q.GetRunningChargeByReference(ctx, byReference); err != nil {
		return nil, err
	}
	now := instant(txSvc.now())
	if row.StoppedAt != nil {
		if in.StoppedAt != nil && !row.StoppedAt.Equal(instant(*in.StoppedAt)) {
			return nil, runningChargeTermsChanged(reference, "stopped_at", *row.StoppedAt, *in.StoppedAt)
		}
		return runningChargeFromRow(row, now)
	}
	stoppedAt := now
	if in.StoppedAt != nil {
		stoppedAt = instant(*in.StoppedAt)
	}
	if stoppedAt.After(now.Add(runningChargeClockSkew)) {
		return nil, runningChargeInvalid("stopped_at", "stopped_at is more than %d seconds ahead", int(runningChargeClockSkew/time.Second))
	}
	if stoppedAt.Before(row.StartedAt) {
		return nil, runningChargeInvalid("stopped_at", "stopped_at is before the charge started")
	}
	// A lot that lapsed before the stop funds what accrued until it lapsed:
	// it is settled and expired first, as the expiry sweep would.
	if _, err := txSvc.grantLedger(q, merchantID.UUID()).ExpireLapsedBy(ctx, customer.UUID(), row.Currency, earlier(stoppedAt, now)); err != nil {
		return nil, err
	}
	if row, err = q.GetRunningChargeByReference(ctx, byReference); err != nil {
		return nil, err
	}
	if stoppedAt.Before(row.PostedThroughAt) {
		return nil, runningChargeInvalid("stopped_at", "stopped_at is before the charge was last posted")
	}
	if row, err = txSvc.postRunningCharge(ctx, q, row, stoppedAt, true); err != nil {
		return nil, err
	}
	if err := txSvc.watchBalance(ctx, q, merchantID.UUID(), customer.UUID(), row.Currency); err != nil {
		return nil, err
	}
	return runningChargeFromRow(row, now)
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// GetRunningCharge reads one charge by the host's reference.
func (s *MoneyService) GetRunningCharge(ctx context.Context, reference string) (*billing.RunningCharge, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *billing.RunningCharge
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, err := s.db.Gen(ctx).GetRunningChargeByReference(ctx, gen.GetRunningChargeByReferenceParams{MerchantID: merchantID.UUID(), Reference: reference})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRunningChargeNotFound
		}
		if err != nil {
			return err
		}
		out, err = runningChargeFromRow(row, instant(s.now()))
		return err
	})
	return out, err
}

// runningChargeCursor is a list position: ids are uuidv7, newest first.
type runningChargeCursor struct {
	ID uuid.UUID `json:"i"`
}

// ListRunningCharges pages the merchant's charges, newest first.
func (s *MoneyService) ListRunningCharges(ctx context.Context, params billing.RunningChargeListParams) (billing.ListPage[billing.RunningCharge], error) {
	var page billing.ListPage[billing.RunningCharge]
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	var after runningChargeCursor
	present, err := pagination.Decode(params.Cursor, &after)
	if err != nil {
		return page, err
	}
	if present && after.ID == uuid.Nil {
		return page, pagination.ErrInvalidCursor
	}
	args := gen.ListRunningChargesParams{RowLimit: pagination.Fetch(limit)}
	if present {
		args.AfterID = &after.ID
	}
	switch params.State {
	case "":
	case billing.RunningChargeRunning, billing.RunningChargeStopped:
		running := params.State == billing.RunningChargeRunning
		args.Running = &running
	default:
		return page, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, `state must be "running" or "stopped"`).WithParam("state")
	}
	if !params.CustomerID.IsZero() {
		customer := params.CustomerID.UUID()
		args.CustomerID = &customer
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	args.MerchantID = merchantID.UUID()
	now := instant(s.now())
	var rows []gen.BillingRunningCharge
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if !params.Depleted {
			rows, err = s.db.Gen(ctx).ListRunningCharges(ctx, args)
			return err
		}
		// Only a running charge can be of a depleted customer.
		if args.Running != nil && !*args.Running {
			return nil
		}
		depleted, err := s.db.Gen(ctx).ListDepletedRunningCharges(ctx, gen.ListDepletedRunningChargesParams{
			MerchantID: args.MerchantID, Now: now, CustomerID: args.CustomerID, AfterID: args.AfterID, RowLimit: args.RowLimit,
		})
		for _, row := range depleted {
			rows = append(rows, row.BillingRunningCharge)
		}
		return err
	}); err != nil {
		return page, err
	}
	items := make([]billing.RunningCharge, 0, len(rows))
	for _, row := range rows {
		charge, err := runningChargeFromRow(row, now)
		if err != nil {
			return page, err
		}
		items = append(items, *charge)
	}
	return pagination.Cut(items, limit, func(c billing.RunningCharge) any { return runningChargeCursor{ID: c.ID.UUID()} }), nil
}

// runningChargeFromRow is the charge as of now: a running one has charged its
// total through now, a stopped one its total through the stop.
func runningChargeFromRow(row gen.BillingRunningCharge, now time.Time) (*billing.RunningCharge, error) {
	out := &billing.RunningCharge{
		ID: billing.RunningChargeID(row.ID), Reference: row.Reference, CustomerID: billing.CustomerID(row.CustomerID),
		Currency: row.Currency, RatePerHour: row.RatePerHour, State: billing.RunningChargeRunning,
		StartedAt: row.StartedAt.UTC(), ChargedAmount: row.PostedAmount, PostedAmount: row.PostedAmount, AsOf: now,
		OverdraftAmount: row.OverdraftAmount, Resource: row.Resource,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
	if row.StoppedAt != nil {
		stoppedAt := row.StoppedAt.UTC()
		out.State, out.StoppedAt, out.AsOf = billing.RunningChargeStopped, &stoppedAt, stoppedAt
		return out, nil
	}
	total, err := RunningChargeTotal(row.RatePerHour, row.StartedAt, now)
	if err != nil {
		return nil, err
	}
	// A posting may run ahead of now by the clock skew a host's instant is allowed.
	out.ChargedAmount = max(total, row.PostedAmount)
	return out, nil
}

// runningChargeLines are the lines a customer's running charges draw on its
// balance: their rates summed and the smallest overdraft.
type runningChargeLines struct {
	ratePerHour, overdraft int64
}

func runningLines(charges []gen.BillingRunningCharge) runningChargeLines {
	var lines runningChargeLines
	for i, c := range charges {
		lines.ratePerHour = satAdd(lines.ratePerHour, c.RatePerHour)
		if i == 0 || c.OverdraftAmount < lines.overdraft {
			lines.overdraft = c.OverdraftAmount
		}
	}
	return lines
}

// capacityFloor is the available balance at which a customer's capacity is
// zero, and what it owes (capacityFloors).
func (s *MoneyService) capacityFloor(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, overdraft int64) (floor, owed int64, err error) {
	if owed, err = s.moneyLedger(q, merchantID).OutstandingOwed(ctx, customer, currency); err != nil {
		return 0, 0, err
	}
	settings, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, err
	}
	var row *gen.BillingMoneySetting
	if err == nil {
		row = &settings
	}
	floor, _, _ = capacityFloors(row, owed, overdraft)
	return floor, owed, nil
}

// lapsedUnspent is what lapsed lots the expiry sweep has not reached will
// expire with: each first funds what the running charges accrued until it
// lapsed, as the sweep posts it (grants.ExpireLapsedBy).
func lapsedUnspent(charges []gen.BillingRunningCharge, lapsed []gen.ListLapsedCreditLotsRow) (int64, error) {
	posted := make([]int64, len(charges))
	for i, c := range charges {
		posted[i] = c.PostedAmount
	}
	var unspent int64
	for _, lot := range lapsed {
		if lot.Remaining <= 0 {
			continue
		}
		var accrued int64
		for i, c := range charges {
			total, err := RunningChargeTotal(c.RatePerHour, c.StartedAt, lot.EndsAt)
			if err != nil {
				return 0, err
			}
			if total > posted[i] {
				accrued = satAdd(accrued, total-posted[i])
				posted[i] = total
			}
		}
		unspent = satAdd(unspent, max(satSub(lot.Remaining, accrued), 0))
	}
	return unspent, nil
}

// PublicBalance is bal as a customer's money in one currency: what its
// running charges have accrued apart from its other holds, the rate they draw
// it at and when its capacity reaches zero. It reads through s's own
// connection.
func (s *MoneyService) PublicBalance(ctx context.Context, bal *models.MoneyBalance, mode billing.BillingMode, owed int64) (*billing.Balance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	out := &billing.Balance{
		CustomerID: billing.CustomerID(bal.CustomerID), Currency: bal.Currency, BillingMode: mode,
		BalanceAmount: bal.Balance, HeldAmount: bal.HeldBalance - bal.Accrued, AccruedAmount: bal.Accrued,
		AvailableAmount: bal.Balance - bal.HeldBalance, OwedAmount: owed, AsOf: bal.AsOf,
	}
	view, err := s.viewBalance(ctx, s.db.Gen(ctx), bal)
	if err != nil || len(view.charges) == 0 {
		return out, err
	}
	out.AccrualRatePerHour, out.DepletesAt = view.accrualRate, view.project(lowBalanceLine{}, 0).depletesAt
	return out, nil
}
