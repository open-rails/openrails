//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// refusedParam is the param a refusal names.
func refusedParam(t *testing.T, err error) string {
	t.Helper()
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.NotNil(t, refusal.Param, "%v", err)
	return *refusal.Param
}

// redisRuntime is a merchant's in-process client with the disposable Redis
// wasted-spend tracking needs.
func (f *fixture) redisRuntime(t *testing.T, slug string) *openrails.Client {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug}
	cfg.Redis = &openrails.RedisConfig{Addr: addr}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	return client
}

func balanceOf(t *testing.T, client *openrails.Client, customer billing.CustomerID) int64 {
	t.Helper()
	balance, err := client.GetBalance(t.Context(), customer, "USD")
	require.NoError(t, err)
	return balance.BalanceAmount
}

// Credit grants are a batch across customers, all or none: a bad or changed
// item grants nothing, an exact retry replays every grant, and another
// merchant's grants never touch this merchant's customers.
func TestCreditGrantBatchesAreAllOrNone(t *testing.T) {
	f := newFixture(t)
	a := f.runtime(t, "grants-a-"+uuid.NewString()[:8])
	b := f.runtime(t, "grants-b-"+uuid.NewString()[:8])
	ctx := t.Context()
	x, y := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	item := func(customer billing.CustomerID, amount int64, sourceID string) billing.CreateCreditGrantParams {
		return billing.CreateCreditGrantParams{CustomerID: customer, Currency: "usd", Amount: amount, Source: "support", SourceID: sourceID}
	}

	batch := []billing.CreateCreditGrantParams{item(x, 1_000_000, "x-1"), item(y, 2_000_000, "y-1"), item(x, 500_000, "x-2")}
	grants, err := a.CreateCreditGrants(ctx, batch)
	require.NoError(t, err)
	require.Len(t, grants, 3)
	for i, grant := range grants {
		require.Equal(t, batch[i].CustomerID, grant.CustomerID, "request order")
		require.Equal(t, batch[i].Amount, grant.Amount)
		require.False(t, grant.Replayed)
	}
	require.EqualValues(t, 1_500_000, balanceOf(t, a, x))
	require.EqualValues(t, 2_000_000, balanceOf(t, a, y))

	again, err := a.CreateCreditGrants(ctx, batch)
	require.NoError(t, err)
	for i, grant := range again {
		require.True(t, grant.Replayed, "item %d", i)
		require.Equal(t, grants[i].ID, grant.ID)
	}
	require.EqualValues(t, 1_500_000, balanceOf(t, a, x), "a retry moves nothing")

	// One bad item refuses the batch: the good one before it is not granted.
	_, err = a.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{item(x, 1, "x-3"), {CustomerID: y, Currency: "XXQ", Amount: 1, Source: "support", SourceID: "y-2"}})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.Equal(t, "items[1].currency", refusedParam(t, err))
	// A changed retry of one item refuses the batch, whatever order it ran.
	_, err = a.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{item(y, 1, "y-3"), item(x, 999, "x-1")})
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	require.Equal(t, "items[1].source_id", refusedParam(t, err))
	// One source named twice is refused before anything is written.
	_, err = a.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{item(x, 1, "x-4"), item(x, 1, "x-4")})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.Equal(t, "items[1].source_id", refusedParam(t, err))
	for _, source := range []string{"x-3", "x-4"} {
		page, err := a.ListCreditGrants(ctx, x, billing.CreditGrantListParams{SourceID: source})
		require.NoError(t, err)
		require.Empty(t, page.Items, "%s was rolled back", source)
	}
	page, err := a.ListCreditGrants(ctx, y, billing.CreditGrantListParams{SourceID: "y-3"})
	require.NoError(t, err)
	require.Empty(t, page.Items, "y-3 was rolled back")
	require.EqualValues(t, 1_500_000, balanceOf(t, a, x))
	require.EqualValues(t, 2_000_000, balanceOf(t, a, y))

	_, err = a.CreateCreditGrants(ctx, make([]billing.CreateCreditGrantParams, billing.MaxBatchItems+1))
	require.ErrorIs(t, err, billing.ErrInvalid)

	// The same customer id at another merchant is another customer.
	other, err := b.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{item(x, 7, "x-1")})
	require.NoError(t, err)
	require.False(t, other[0].Replayed)
	require.NotEqual(t, grants[0].ID, other[0].ID)
	require.EqualValues(t, 7, balanceOf(t, b, x))
	require.EqualValues(t, 1_500_000, balanceOf(t, a, x))
	listed, err := b.ListCreditGrants(ctx, x, billing.CreditGrantListParams{IDs: []billing.CreditGrantID{grants[0].ID, grants[2].ID, other[0].ID}})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1, "a merchant reads only its own grants")
	require.Equal(t, other[0].ID, listed.Items[0].ID)
}

// Admissions release and extend in batches, one result per item: each item
// is decided on its own, a retry answers the same, and another merchant's
// request ids are unknown.
func TestAdmissionBatchesAnswerPerItem(t *testing.T) {
	f := newFixture(t)
	a := f.runtime(t, "holds-a-"+uuid.NewString()[:8])
	b := f.runtime(t, "holds-b-"+uuid.NewString()[:8])
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := createCreditGrant(ctx, a, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"})
	require.NoError(t, err)
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	admit := func(requestID string) {
		t.Helper()
		verdicts, err := a.Admit(ctx, []billing.AdmitParams{{
			RequestID: requestID, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline,
		}})
		require.NoError(t, err)
		require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	}
	r1, r2, r3 := "r1-"+uuid.NewString(), "r2-"+uuid.NewString(), "r3-"+uuid.NewString()
	for _, id := range []string{r1, r2, r3} {
		admit(id)
	}
	_, err = a.CaptureAdmission(ctx, r3, billing.CaptureAdmissionParams{Amount: 50_000})
	require.NoError(t, err)

	released, err := a.ReleaseAdmissions(ctx, []string{r1, "unknown-" + uuid.NewString(), r3, r1})
	require.NoError(t, err)
	require.Len(t, released, 4)
	require.Equal(t, http.StatusOK, released[0].Status)
	require.Equal(t, billing.AdmissionReleased, *released[0].Admission.State)
	require.Equal(t, http.StatusNotFound, released[1].Status)
	require.Equal(t, "admission_not_found", released[1].Error.Code)
	require.ErrorIs(t, released[2].Err(), billing.ErrConflict, "a captured admission stays captured")
	require.Equal(t, "admission_captured", released[2].Error.Code)
	require.Equal(t, http.StatusOK, released[3].Status, "releasing again answers the released admission")
	retry, err := a.ReleaseAdmissions(ctx, []string{r1})
	require.NoError(t, err)
	require.Equal(t, billing.AdmissionReleased, *retry[0].Admission.State)

	// Another merchant cannot see, release or extend this merchant's holds.
	foreign, err := b.ReleaseAdmissions(ctx, []string{r2})
	require.NoError(t, err)
	require.Equal(t, "admission_not_found", foreign[0].Error.Code)
	later := deadline.Add(time.Hour)
	foreignExtend, err := b.ExtendAdmissions(ctx, []billing.ExtendAdmissionParams{{RequestID: r2, ExpiresAt: later}})
	require.NoError(t, err)
	require.Equal(t, "admission_not_found", foreignExtend[0].Error.Code)
	open, err := a.GetAdmission(ctx, r2)
	require.NoError(t, err)
	require.Equal(t, billing.AdmissionOpen, *open.State)
	require.True(t, open.ExpiresAt.Equal(deadline), "untouched by the other merchant")

	extended, err := a.ExtendAdmissions(ctx, []billing.ExtendAdmissionParams{
		{RequestID: r2, ExpiresAt: later}, {RequestID: r1, ExpiresAt: later}, {RequestID: " ", ExpiresAt: later}, {RequestID: r2, ExpiresAt: later},
	})
	require.NoError(t, err)
	require.Len(t, extended, 4)
	require.Equal(t, http.StatusOK, extended[0].Status, "%+v", extended[0].Error)
	require.True(t, extended[0].Admission.ExpiresAt.Equal(later))
	require.Equal(t, "hold_not_found", extended[1].Error.Code, "a released hold cannot be extended")
	require.Equal(t, http.StatusBadRequest, extended[2].Status)
	require.Equal(t, http.StatusOK, extended[3].Status, "the same deadline again is a retry: %+v", extended[3].Error)
	require.True(t, extended[3].Admission.ExpiresAt.Equal(later))

	_, err = a.ReleaseAdmissions(ctx, make([]string, billing.MaxAdmissionBatchItems+1))
	require.ErrorIs(t, err, billing.ErrInvalid)
	_, err = a.ExtendAdmissions(ctx, nil)
	require.ErrorIs(t, err, billing.ErrInvalid)
}

// Wasted spend is reported in batches, one result per report: a bad report
// is refused alone, a retry is a duplicate, and another merchant's reports
// never touch this merchant's customers.
func TestWastedSpendBatchesAnswerPerItem(t *testing.T) {
	f := newFixture(t)
	a := f.redisRuntime(t, "waste-a-"+uuid.NewString()[:8])
	b := f.redisRuntime(t, "waste-b-"+uuid.NewString()[:8])
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := createCreditGrant(ctx, a, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"})
	require.NoError(t, err)
	report := func(sourceID string, amount int64) billing.ReportWastedSpendParams {
		return billing.ReportWastedSpendParams{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "usd", Amount: amount, Source: "worker", SourceID: sourceID}
	}

	results, err := a.ReportWastedSpend(ctx, []billing.ReportWastedSpendParams{report("w-1", 10_000), report("w-2", -1), report("w-3", 20_000)})
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, http.StatusOK, results[0].Status, "%+v", results[0].Error)
	require.NotEqual(t, billing.WastedSpendDuplicate, results[0].Report.Action)
	require.ErrorIs(t, results[1].Err(), billing.ErrInvalid)
	require.Equal(t, "amount", *results[1].Error.Param)
	require.Equal(t, http.StatusOK, results[2].Status, "a bad report refuses only itself")
	after := balanceOf(t, a, customer)

	retry, err := a.ReportWastedSpend(ctx, []billing.ReportWastedSpendParams{report("w-1", 10_000), report("w-1", 11_000)})
	require.NoError(t, err)
	require.Equal(t, billing.WastedSpendDuplicate, retry[0].Report.Action, "a retry is handled once")
	require.True(t, errors.Is(retry[1].Err(), billing.ErrIdempotencyKeyReused), "%+v", retry[1].Error)
	require.Equal(t, after, balanceOf(t, a, customer), "money never moves twice")

	foreign, err := b.ReportWastedSpend(ctx, []billing.ReportWastedSpendParams{report("w-1", 10_000)})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, foreign[0].Status)
	require.NotEqual(t, billing.WastedSpendDuplicate, foreign[0].Report.Action, "another merchant's report is its own")
	require.Equal(t, after, balanceOf(t, a, customer))

	_, err = a.ReportWastedSpend(ctx, make([]billing.ReportWastedSpendParams, billing.MaxBatchItems+1))
	require.ErrorIs(t, err, billing.ErrInvalid)
}
