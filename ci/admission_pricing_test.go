//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// admitUsage admits one request whose hold is its usage estimate's price,
// due an hour after the test began.
func (p *pricing) admitUsage(customer billing.CustomerID, requestID string, estimate billing.UsageEstimate) billing.AdmissionVerdict {
	p.t.Helper()
	deadline := p.started.Add(time.Hour)
	verdicts, err := p.client.Admit(p.ctx, []billing.AdmitParams{{RequestID: requestID, CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", Usage: &estimate, ExpiresAt: &deadline}})
	require.NoError(p.t, err)
	return verdicts[0]
}

func (p *pricing) capture(items ...billing.CaptureAdmissionParams) []billing.CaptureResult {
	p.t.Helper()
	results, err := p.client.CaptureAdmissions(p.ctx, items)
	require.NoError(p.t, err)
	return results
}

// captured settles one request with its usage and answers the receipt.
func (p *pricing) captured(requestID, eventType string, dimensions map[string]int64) *billing.CaptureReceipt {
	p.t.Helper()
	result := p.capture(billing.CaptureAdmissionParams{RequestID: requestID, Usage: &billing.CaptureUsage{EventType: eventType, Dimensions: dimensions}})[0]
	require.Equal(p.t, http.StatusOK, result.Status, "%+v", result.Error)
	return result.Receipt
}

func receiptCharges(r *billing.CaptureReceipt) map[string]int64 {
	out := map[string]int64{}
	for _, c := range r.Charges {
		out[c.MeterKey] += c.Amount
	}
	return out
}

// An admission's usage estimate is priced into its hold by the rate cards: a
// call at its per-call price, an LLM request at its prompt and max tokens.
func TestHoldIsPricedFromTheEstimatedUsage(t *testing.T) {
	p := newPricing(t, newFixture(t), "hold")
	p.meter("calls", "endpoint-a", catalog.AggregationCount, "", nil, perUnit(10_000, 0), nil)
	p.meter("input-tokens", "chat", catalog.AggregationSum, "input_tokens", nil, perUnit(150_000, 1_000_000), nil)
	p.meter("output-tokens", "chat", catalog.AggregationSum, "output_tokens", nil, perUnit(600_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	call := p.admitUsage(customer, "call-1", billing.UsageEstimate{EventType: "endpoint-a"})
	require.True(t, call.Allowed(), "%+v", call)
	require.EqualValues(t, 10_000, call.Admission.EstimatedAmount)
	// 4,000 prompt tokens and 1,000 max tokens: 600 + 600.
	chat := p.admitUsage(customer, "chat-1", billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"input_tokens": 4_000, "output_tokens": 1_000}})
	require.True(t, chat.Allowed(), "%+v", chat)
	require.EqualValues(t, 1_200, chat.Admission.EstimatedAmount)
	balance := p.balance(customer)
	require.EqualValues(t, 11_200, balance.HeldAmount)
	require.EqualValues(t, 1_000_000-11_200, balance.AvailableAmount)

	// A replay answers the admission as it stands, even after a price change.
	p.meter("calls", "endpoint-a", catalog.AggregationCount, "", nil, perUnit(90_000, 0), nil)
	replay := p.admitUsage(customer, "call-1", billing.UsageEstimate{EventType: "endpoint-a"})
	require.True(t, replay.Admission.Replayed)
	require.EqualValues(t, 10_000, replay.Admission.EstimatedAmount)
	changed := p.admitUsage(customer, "call-1", billing.UsageEstimate{EventType: "endpoint-a", Dimensions: map[string]int64{"x": 1}})
	require.Equal(t, billing.CodeIdempotencyKeyReused, changed.Error.Code, "%+v", changed)

	// Usage and a host-stated estimate are exclusive; an unpriced event type
	// is not an estimate of money.
	deadline := time.Now().Add(time.Hour)
	verdicts, err := p.client.Admit(p.ctx, []billing.AdmitParams{
		{RequestID: "both", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			EstimatedAmount: 5, Usage: &billing.UsageEstimate{EventType: "endpoint-a"}, ExpiresAt: &deadline},
		{RequestID: "unpriced", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
			Usage: &billing.UsageEstimate{EventType: "nothing"}, ExpiresAt: &deadline},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, verdicts[0].Status)
	require.Equal(t, "usage", *verdicts[0].Error.Param)
	require.Equal(t, http.StatusUnprocessableEntity, verdicts[1].Status)
	require.Equal(t, billing.CodeUsageNotPriced, verdicts[1].Error.Code)
}

func TestUsageEstimateCannotOverflowIntoAFreeAdmission(t *testing.T) {
	p := newPricing(t, newFixture(t), "estimate-overflow")
	// Each card's price fits int64; their sum is 2^64, which must not
	// wrap to zero and admit the request without reserving any money.
	for _, key := range []string{"first", "second", "third", "fourth"} {
		p.meter(key, "expensive", catalog.AggregationCount, "", nil, perUnit(1<<62, 1), nil)
	}
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	verdict := p.admitUsage(customer, "overflow", billing.UsageEstimate{EventType: "expensive"})
	require.False(t, verdict.Allowed(), "an overflowing estimate must not create a free admission")
	require.NotNil(t, verdict.Error)
	require.Nil(t, verdict.Admission)
	require.Zero(t, p.balance(customer).HeldAmount)
}

// Adjacent int64 quantities above float64's exact range are different terms,
// even when both estimates round to the same hold amount.
func TestUsageEstimateReplayPreservesExactQuantities(t *testing.T) {
	p := newPricing(t, newFixture(t), "estimate-identity")
	p.meter("tokens", "large", catalog.AggregationSum, "tokens", nil, perUnit(1, 1<<53), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	estimate := billing.UsageEstimate{EventType: "large", Dimensions: map[string]int64{"tokens": 1 << 53},
		Metadata: map[string]any{"scale": 1e21}}
	first := p.admitUsage(customer, "exact", estimate)
	require.True(t, first.Allowed(), "%+v", first)
	require.EqualValues(t, 1, first.Admission.EstimatedAmount)
	replay := p.admitUsage(customer, "exact", estimate)
	require.True(t, replay.Allowed(), "%+v", replay)
	require.True(t, replay.Admission.Replayed, "jsonb's numeric spelling must preserve an exact replay")
	estimate.Dimensions["tokens"]++
	changed := p.admitUsage(customer, "exact", estimate)
	require.False(t, changed.Allowed(), "a different int64 quantity must not replay the first estimate")
	require.NotNil(t, changed.Error)
	require.Equal(t, billing.CodeIdempotencyKeyReused, changed.Error.Code)
	require.EqualValues(t, 1, p.balance(customer).HeldAmount)
}

// The owner's case: hold $0.02, the work costs $0.01, and $0.01 moves. The
// hold is gone and the usage is recorded with its charges.
func TestCaptureSettlesAtTheCatalogPrice(t *testing.T) {
	p := newPricing(t, newFixture(t), "settle")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	admitted := p.admitUsage(customer, "req-1", billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 2}})
	require.EqualValues(t, 20_000, admitted.Admission.EstimatedAmount)
	require.EqualValues(t, 20_000, p.balance(customer).HeldAmount)

	receipt := p.captured("req-1", "gen", map[string]int64{"units": 1})
	require.EqualValues(t, 20_000, receipt.EstimatedAmount)
	require.EqualValues(t, 10_000, receipt.Amount)
	require.Equal(t, map[string]int64{"units": 10_000}, receiptCharges(receipt))
	require.NotNil(t, receipt.UsageEventID)
	require.NotNil(t, receipt.BalanceTransactionID)
	require.False(t, receipt.Replayed)
	balance := p.balance(customer)
	require.EqualValues(t, 0, balance.HeldAmount)
	require.EqualValues(t, 990_000, balance.BalanceAmount)

	// The next event continues the period's quantity the capture advanced.
	next := p.record(usage(customer, "gen", map[string]int64{"units": 1}, nil))
	require.EqualValues(t, 10_000, next.Amount)
}

// A capture priced above its hold posts in full: the rest is owed, never
// clamped. The host bounds it by its estimate.
func TestCaptureAboveTheHoldIsOwed(t *testing.T) {
	p := newPricing(t, newFixture(t), "above")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 25_000)

	admitted := p.admitUsage(customer, "req-1", billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 2}})
	require.True(t, admitted.Allowed(), "%+v", admitted)
	receipt := p.captured("req-1", "gen", map[string]int64{"units": 5})
	require.EqualValues(t, 50_000, receipt.Amount)
	balance := p.balance(customer)
	require.EqualValues(t, 0, balance.BalanceAmount)
	require.EqualValues(t, 25_000, balance.OwedAmount)
	require.EqualValues(t, 0, balance.HeldAmount)
}

// A request pays the price it was admitted at: a capture is priced by the
// cards pinned at admission, not the ones in force. An admission that
// pinned none, a host-stated estimate, is priced by the cards in force.
func TestCaptureUsesTheAdmittedRateCard(t *testing.T) {
	p := newPricing(t, newFixture(t), "pinned")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)

	p.admitUsage(customer, "pinned", billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 1}})
	deadline := time.Now().Add(time.Hour)
	_, err := p.client.Admit(p.ctx, []billing.AdmitParams{{RequestID: "stated", CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 10_000, ExpiresAt: &deadline}})
	require.NoError(t, err)
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(30_000, 0), nil)

	require.EqualValues(t, 10_000, p.captured("pinned", "gen", map[string]int64{"units": 1}).Amount)
	require.EqualValues(t, 30_000, p.captured("stated", "gen", map[string]int64{"units": 1}).Amount)
	require.EqualValues(t, 30_000, p.record(usage(customer, "gen", map[string]int64{"units": 1}, nil)).Amount)
}

// A capture batch decides each item on its own, as capturing it alone would.
func TestCaptureBatchDecidesEachItem(t *testing.T) {
	p := newPricing(t, newFixture(t), "batch")
	model := catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{
		Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: 1_000}}}}}
	p.meter("units", "gen", catalog.AggregationSum, "units", map[string]string{"model": "metadata.model"}, model, nil)
	p.meter("calls", "other", catalog.AggregationCount, "", nil, perUnit(500, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	small := map[string]any{"model": "small"}
	for _, id := range []string{"ok", "wrong-type", "gap"} {
		require.True(t, p.admitUsage(customer, id, billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 1}, Metadata: small}).Allowed())
	}

	results := p.capture(
		billing.CaptureAdmissionParams{RequestID: "ok", Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 3}, Metadata: small}},
		billing.CaptureAdmissionParams{RequestID: "missing", Usage: &billing.CaptureUsage{EventType: "gen"}},
		billing.CaptureAdmissionParams{RequestID: "wrong-type", Usage: &billing.CaptureUsage{EventType: "other"}},
		billing.CaptureAdmissionParams{RequestID: "gap", Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 1}, Metadata: map[string]any{"model": "huge"}}},
		billing.CaptureAdmissionParams{RequestID: "ok"},
	)
	require.Equal(t, http.StatusOK, results[0].Status, "%+v", results[0].Error)
	require.EqualValues(t, 3_000, results[0].Receipt.Amount)
	require.Equal(t, http.StatusNotFound, results[1].Status)
	require.Equal(t, "admission_not_found", results[1].Error.Code)
	require.Equal(t, http.StatusBadRequest, results[2].Status)
	require.Equal(t, "usage.event_type", *results[2].Error.Param)
	require.Equal(t, http.StatusUnprocessableEntity, results[3].Status)
	require.Equal(t, billing.CodeUsageNotPriced, results[3].Error.Code)
	require.Equal(t, "usage.metadata.model", *results[3].Error.Param)
	require.Equal(t, http.StatusBadRequest, results[4].Status, "a capture names its usage or an amount")

	// The refused items are still open; they settle once their terms are right.
	fixed := p.capture(billing.CaptureAdmissionParams{RequestID: "gap", Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 1}, Metadata: small}})[0]
	require.Equal(t, http.StatusOK, fixed.Status, "%+v", fixed.Error)
	require.EqualValues(t, 1_000, fixed.Receipt.Amount)
	balance := p.balance(customer)
	require.EqualValues(t, 1_000_000-3_000-1_000, balance.BalanceAmount)
	require.EqualValues(t, 1_000, balance.HeldAmount, "wrong-type still holds its estimate")
}

// The first capture fixes its terms and charge: an exact retry answers that
// receipt, even after a price change, and changed usage is refused.
func TestCaptureRetryAnswersTheFirstReceipt(t *testing.T) {
	p := newPricing(t, newFixture(t), "retry")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	p.admitUsage(customer, "req-1", billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 1}})

	first := p.captured("req-1", "gen", map[string]int64{"units": 2})
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(50_000, 0), nil)
	again := p.captured("req-1", "gen", map[string]int64{"units": 2})
	require.True(t, again.Replayed)
	first.Replayed = true
	require.Equal(t, first, again)
	changed := p.capture(billing.CaptureAdmissionParams{RequestID: "req-1", Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 3}}})[0]
	require.ErrorIs(t, changed.Err(), billing.ErrIdempotencyKeyReused)
	stated := int64(20_000)
	changed = p.capture(billing.CaptureAdmissionParams{RequestID: "req-1", Amount: &stated, Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 2}}})[0]
	require.ErrorIs(t, changed.Err(), billing.ErrIdempotencyKeyReused, "a host-stated amount is another term")
	require.EqualValues(t, 1_000_000-20_000, p.balance(customer).BalanceAmount)
}

// A capture's usage is of the event type the request was admitted for.
func TestCaptureEventTypeMustMatchTheAdmission(t *testing.T) {
	p := newPricing(t, newFixture(t), "match")
	p.meter("a-calls", "endpoint-a", catalog.AggregationCount, "", nil, perUnit(10_000, 0), nil)
	p.meter("b-calls", "endpoint-b", catalog.AggregationCount, "", nil, perUnit(20_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	p.admitUsage(customer, "req-1", billing.UsageEstimate{EventType: "endpoint-a"})

	result := p.capture(billing.CaptureAdmissionParams{RequestID: "req-1", Usage: &billing.CaptureUsage{EventType: "endpoint-b"}})[0]
	require.ErrorIs(t, result.Err(), billing.ErrInvalid)
	require.Equal(t, billing.CodeInvalidParam, result.Error.Code)
	require.Equal(t, "usage.event_type", *result.Error.Param)
	require.EqualValues(t, 10_000, p.captured("req-1", "endpoint-a", nil).Amount)
}

// An admission inside the host's transaction is the host's: rolled back with
// it, it holds nothing and the request id is free; committed, it holds.
func TestAdmitTxRollsBackWithTheHost(t *testing.T) {
	f := newFixture(t)
	p := newPricing(t, f, "admit-tx")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	deadline := time.Now().Add(time.Hour)
	params := billing.AdmitParams{RequestID: "req-1", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
		Currency: "USD", Usage: &billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 3}}, ExpiresAt: &deadline}
	admitTx := func(commit bool) billing.AdmissionVerdict {
		t.Helper()
		tx, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		verdicts, err := p.client.AdmitTx(p.ctx, tx, []billing.AdmitParams{params})
		require.NoError(t, err)
		if commit {
			require.NoError(t, tx.Commit(t.Context()))
		}
		return verdicts[0]
	}
	rolledBack := admitTx(false)
	require.True(t, rolledBack.Allowed(), "%+v", rolledBack)
	require.EqualValues(t, 30_000, rolledBack.Admission.EstimatedAmount)
	require.EqualValues(t, 0, p.balance(customer).HeldAmount, "a rolled-back admission holds nothing")
	committed := admitTx(true)
	require.False(t, committed.Admission.Replayed, "the rolled-back request id was never claimed")
	require.EqualValues(t, 30_000, p.balance(customer).HeldAmount)

	remote, err := openrails.NewRemoteMerchant("https://openrails.invalid", openrails.RemoteCredential{APIKey: "key"})
	require.NoError(t, err)
	_, err = remote.AdmitTx(p.ctx, nil, []billing.AdmitParams{params})
	require.ErrorIs(t, err, openrails.ErrRemoteClient)
	_, err = remote.CaptureAdmissionsTx(p.ctx, nil, []billing.CaptureAdmissionParams{{RequestID: "req-1"}})
	require.ErrorIs(t, err, openrails.ErrRemoteClient)
	_, err = remote.ReleaseAdmissionsTx(p.ctx, nil, []string{"req-1"})
	require.ErrorIs(t, err, openrails.ErrRemoteClient)
}

// A capture or release inside the host's transaction commits with the host's
// record of the work, or not at all.
func TestCaptureTxCommitsWithTheHost(t *testing.T) {
	f := newFixture(t)
	p := newPricing(t, f, "capture-tx")
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, perUnit(10_000, 0), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	requests := pgx.Identifier{f.schema, "host_requests"}.Sanitize()
	_, err := f.pool.Exec(t.Context(), "CREATE TABLE "+requests+" (id text PRIMARY KEY, state text NOT NULL)")
	require.NoError(t, err)
	state := func(id string) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(t.Context(), "SELECT state FROM "+requests+" WHERE id = $1", id).Scan(&s); err == pgx.ErrNoRows {
			return ""
		}
		return s
	}
	for _, id := range []string{"done", "failed"} {
		require.True(t, p.admitUsage(customer, id, billing.UsageEstimate{EventType: "gen", Dimensions: map[string]int64{"units": 2}}).Allowed())
		_, err := f.pool.Exec(t.Context(), "INSERT INTO "+requests+" (id, state) VALUES ($1, 'running')", id)
		require.NoError(t, err)
	}
	finish := func(commit bool) []billing.CaptureResult {
		t.Helper()
		tx, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(t.Context(), "UPDATE "+requests+" SET state = 'done' WHERE id = 'done'")
		require.NoError(t, err)
		results, err := p.client.CaptureAdmissionsTx(p.ctx, tx, []billing.CaptureAdmissionParams{{RequestID: "done", Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 1}}}})
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "UPDATE "+requests+" SET state = 'failed' WHERE id = 'failed'")
		require.NoError(t, err)
		released, err := p.client.ReleaseAdmissionsTx(p.ctx, tx, []string{"failed"})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, released[0].Status, "%+v", released[0].Error)
		if commit {
			require.NoError(t, tx.Commit(t.Context()))
		}
		return results
	}
	rolledBack := finish(false)
	require.Equal(t, http.StatusOK, rolledBack[0].Status, "%+v", rolledBack[0].Error)
	require.EqualValues(t, 10_000, rolledBack[0].Receipt.Amount)
	require.Equal(t, "running", state("done"))
	balance := p.balance(customer)
	require.EqualValues(t, 1_000_000, balance.BalanceAmount, "a rolled-back capture posts nothing")
	require.EqualValues(t, 40_000, balance.HeldAmount, "and both holds stand")

	committed := finish(true)
	require.False(t, committed[0].Receipt.Replayed)
	require.Equal(t, []string{"done", "failed"}, []string{state("done"), state("failed")})
	balance = p.balance(customer)
	require.EqualValues(t, 990_000, balance.BalanceAmount)
	require.EqualValues(t, 0, balance.HeldAmount)
}

// Captures of one customer serialize on its spend lock: 200 at once leave the
// balance, the owed and the period quantity exact.
func TestConcurrentCapturesOfOneCustomerAreExact(t *testing.T) {
	f := newFixture(t)
	p := newPricing(t, f, "concurrent")
	hundred := int64(100)
	p.meter("units", "gen", catalog.AggregationSum, "units", nil, catalog.RatePrice{Model: catalog.ModelTiered, Tiered: &catalog.TieredPrice{
		Mode: catalog.TierModeGraduated, Tiers: []catalog.RateTier{{UpTo: &hundred, UnitAmount: 1_000}, {UnitAmount: 500}}}}, nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 100_000)
	const n = 200
	deadline := time.Now().Add(time.Hour)
	admits := make([]billing.AdmitParams, n)
	for i := range admits {
		admits[i] = billing.AdmitParams{RequestID: "req-" + uuid.NewString(), CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", Usage: &billing.UsageEstimate{EventType: "gen"}, ExpiresAt: &deadline}
	}
	verdicts, err := p.client.Admit(p.ctx, admits)
	require.NoError(t, err)
	for _, v := range verdicts {
		require.True(t, v.Allowed(), "%+v", v)
	}

	var wg sync.WaitGroup
	results := make([]billing.CaptureResult, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			// 200 requests waiting on one customer's lock outrun the pool:
			// database_busy is a retry, and a retry answers the first capture.
			for attempt := 0; ; attempt++ {
				out, err := p.client.CaptureAdmissions(p.ctx, []billing.CaptureAdmissionParams{{RequestID: admits[i].RequestID,
					Usage: &billing.CaptureUsage{EventType: "gen", Dimensions: map[string]int64{"units": 1}}}})
				var busy *billing.StatusError
				if errors.As(err, &busy) && busy.Code == "database_busy" && attempt < 100 {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				if err == nil {
					results[i] = out[0]
				}
				errs[i] = err
				return
			}
		})
	}
	wg.Wait()
	var total int64
	for i := range n {
		require.NoError(t, errs[i])
		require.Equal(t, http.StatusOK, results[i].Status, "%+v", results[i].Error)
		total += results[i].Receipt.Amount
	}
	require.EqualValues(t, 100*1_000+100*500, total)
	balance := p.balance(customer)
	require.EqualValues(t, 0, balance.BalanceAmount)
	require.EqualValues(t, 50_000, balance.OwedAmount)
	require.EqualValues(t, 0, balance.HeldAmount)
	var quantity int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT sum(quantity) FROM "+pgx.Identifier{f.schema, "meter_period_quantities"}.Sanitize()+
		" WHERE customer_id = $1 AND meter_key = 'units'", customer.UUID()).Scan(&quantity))
	require.EqualValues(t, n, quantity)
}

// A long stream states a deadline and extends it; its capture settles at the
// admitted price.
func TestLongStreamExtendsThenCaptures(t *testing.T) {
	p := newPricing(t, newFixture(t), "stream")
	p.meter("output-tokens", "chat", catalog.AggregationSum, "output_tokens", nil, perUnit(600_000, 1_000_000), nil)
	customer := billing.CustomerID(uuid.New())
	p.grant(customer, 1_000_000)
	deadline := time.Now().Add(time.Minute)
	verdicts, err := p.client.Admit(p.ctx, []billing.AdmitParams{{RequestID: "stream", CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", ExpiresAt: &deadline,
		Usage: &billing.UsageEstimate{EventType: "chat", Dimensions: map[string]int64{"output_tokens": 100_000}}}})
	require.NoError(t, err)
	require.EqualValues(t, 60_000, verdicts[0].Admission.EstimatedAmount)
	extended, err := extendAdmission(p.ctx, p.client, "stream", time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, billing.AdmissionOpen, *extended.State)

	receipt := p.captured("stream", "chat", map[string]int64{"output_tokens": 150_000})
	require.EqualValues(t, 90_000, receipt.Amount)
	require.EqualValues(t, 60_000, receipt.EstimatedAmount)
	require.EqualValues(t, 1_000_000-90_000, p.balance(customer).BalanceAmount)
}
