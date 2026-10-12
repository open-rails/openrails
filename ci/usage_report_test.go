//go:build e2e && integration

package ci_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// usageReader reads GET /v1/me/usage as one customer, signed in.
type usageReader struct {
	t     *testing.T
	page  http.Handler
	token string
}

func readUsageAs(t *testing.T, bill *openrails.Engine, customer billing.CustomerID) usageReader {
	t.Helper()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	return usageReader{t: t, page: mux, token: auth.Person(customer.String())}
}

func (u usageReader) get(query url.Values) (int, []byte) {
	u.t.Helper()
	if query.Get("currency") == "" {
		query.Set("currency", "USD")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/me/usage?"+query.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+u.token)
	rec := httptest.NewRecorder()
	u.page.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// report is one page, which must be answered.
func (u usageReader) report(query url.Values) billing.Usage {
	u.t.Helper()
	status, raw := u.get(query)
	require.Equal(u.t, http.StatusOK, status, "%s", raw)
	var fields map[string]json.RawMessage
	require.NoError(u.t, json.Unmarshal(raw, &fields))
	require.Contains(u.t, fields, "next_cursor", "the last page says so with null")
	var out billing.Usage
	require.NoError(u.t, json.Unmarshal(raw, &out))
	return out
}

// all reads every page of a report limit groups at a time.
func (u usageReader) all(query url.Values, limit int) (rows []billing.UsageRow, pages int) {
	u.t.Helper()
	query.Set("limit", strconv.Itoa(limit))
	for {
		page := u.report(query)
		require.LessOrEqual(u.t, len(page.Rows), limit)
		rows, pages = append(rows, page.Rows...), pages+1
		if page.NextCursor == nil {
			return rows, pages
		}
		require.Len(u.t, page.Rows, limit, "only the last page is short")
		query.Set("cursor", *page.NextCursor)
	}
}

// refused is a 400 refusal of query: its code and param.
func (u usageReader) refused(query url.Values) (string, string) {
	u.t.Helper()
	status, raw := u.get(query)
	require.Equal(u.t, http.StatusBadRequest, status, "%s", raw)
	var envelope struct {
		Error billing.ErrorDetails `json:"error"`
	}
	require.NoError(u.t, json.Unmarshal(raw, &envelope))
	param := ""
	if envelope.Error.Param != nil {
		param = *envelope.Error.Param
	}
	return envelope.Error.Code, param
}

func q(pairs ...string) url.Values {
	out := url.Values{}
	for i := 0; i < len(pairs); i += 2 {
		out.Set(pairs[i], pairs[i+1])
	}
	return out
}

// The usage report is a cursor page of a customer's groups, by total amount
// and then key in byte order: its pages read in turn are the unpaged report,
// a cursor reads only the report it came from, and limit is 1 to 1,000
// (default 100).
func TestUsageReportPages(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "usage-"+uuid.NewString()[:8], nil)
	ctx := t.Context()
	declareDynamicMeter(t, client, "gpu", "seconds")
	declareDynamicMeter(t, client, "cpu", "cores")
	declareDynamicMeter(t, client, "call", "units")
	record := func(customer billing.CustomerID, items []billing.RecordUsageParams) {
		t.Helper()
		_, err := createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "seed-" + customer.String()})
		require.NoError(t, err)
		for i := range items {
			items[i].CustomerID, items[i].Currency, items[i].Source = customer, "USD", "worker"
			items[i].SourceID = fmt.Sprintf("u-%d", i)
		}
		results, err := client.RecordUsage(ctx, items)
		require.NoError(t, err)
		for _, r := range results {
			require.Equal(t, http.StatusCreated, r.Status, "%+v", r.Error)
		}
	}

	customer := billing.CustomerID(uuid.New())
	gpu := func(invoker string, amount int64) billing.RecordUsageParams {
		return billing.RecordUsageParams{EventType: "gpu", Invoker: invoker, Amount: new(int64(amount)), Dimensions: map[string]int64{"seconds": amount / 100}}
	}
	cpu := func(invoker string, amount int64) billing.RecordUsageParams {
		return billing.RecordUsageParams{EventType: "cpu", Invoker: invoker, Amount: new(int64(amount)), Dimensions: map[string]int64{"cores": 1}}
	}
	record(customer, []billing.RecordUsageParams{
		gpu("d", 200), gpu("d", 300), cpu("B", 300), gpu("a", 300), cpu("c", 300), gpu("f", 100), cpu("e", 50),
	})
	me := readUsageAs(t, bill, customer)

	t.Run("pages read in turn are the unpaged report", func(t *testing.T) {
		for _, groupBy := range []string{"resource", "invoker", "event_type"} {
			whole := me.report(q("group_by", groupBy, "limit", "1000"))
			require.Nil(t, whole.NextCursor, groupBy)
			paged, pages := me.all(q("group_by", groupBy), 2)
			require.Equal(t, whole.Rows, paged, groupBy)
			require.Equal(t, (len(whole.Rows)+1)/2, pages, groupBy)
		}

		byInvoker := me.report(q("group_by", "invoker"))
		var keys []string
		var amounts []int64
		for _, row := range byInvoker.Rows {
			keys, amounts = append(keys, row.Key), append(amounts, row.Amount)
		}
		require.Equal(t, []string{"d", "B", "a", "c", "f", "e"}, keys, "ties by key in byte order")
		require.Equal(t, []int64{500, 300, 300, 300, 100, 50}, amounts)
		require.EqualValues(t, 2, byInvoker.Rows[0].EventCount)
		require.Nil(t, byInvoker.Rows[0].Dimensions, "dimensions are an event-type report's")
		require.Equal(t, []billing.UsageRow{{Key: "", EventCount: 7, Amount: 1550}}, me.report(q("group_by", "resource")).Rows, "no resource is ''")

		gpuPage := me.report(q("limit", "1"))
		require.Equal(t, []billing.UsageRow{{Key: "gpu", EventCount: 4, Amount: 900, Dimensions: map[string]int64{"seconds": 9}}}, gpuPage.Rows)
		require.NotNil(t, gpuPage.NextCursor)
		cpuPage := me.report(q("limit", "1", "cursor", *gpuPage.NextCursor))
		require.Equal(t, []billing.UsageRow{{Key: "cpu", EventCount: 3, Amount: 650, Dimensions: map[string]int64{"cores": 3}}}, cpuPage.Rows,
			"each page sums its own event types' dimensions")
		require.Nil(t, cpuPage.NextCursor)
	})

	first := me.report(q("group_by", "invoker", "limit", "2"))
	require.NotNil(t, first.NextCursor)
	cursor := *first.NextCursor

	t.Run("a cursor reads the rest of its own report", func(t *testing.T) {
		next := me.report(q("group_by", "invoker", "limit", "2", "cursor", cursor))
		require.True(t, next.From.Equal(first.From) && next.To.Equal(first.To), "a window left out is the cursor's, not the month before this request")
		require.Equal(t, []string{"a", "c"}, []string{next.Rows[0].Key, next.Rows[1].Key})
		named := me.report(q("group_by", "invoker", "limit", "2", "cursor", cursor,
			"from", first.From.Format(time.RFC3339Nano), "to", first.To.Format(time.RFC3339Nano)))
		require.Equal(t, next.Rows, named.Rows, "the same window, named")
	})

	t.Run("a foreign or altered cursor is refused", func(t *testing.T) {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		require.NoError(t, err)
		edit := func(from, to string) string {
			require.Contains(t, string(raw), from)
			return base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), from, to, 1)))
		}
		balance := httptest.NewRequest(http.MethodGet, "/v1/me/balance/transactions?currency=USD&limit=1", nil)
		balance.Header.Set("Authorization", "Bearer "+me.token)
		rec := httptest.NewRecorder()
		me.page.ServeHTTP(rec, balance)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var transactions billing.ListPage[billing.BalanceTransaction]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &transactions))
		require.NotEmpty(t, transactions.Next)

		for name, query := range map[string]url.Values{
			"another list's":       q("group_by", "invoker", "cursor", transactions.Next),
			"another grouping":     q("group_by", "resource", "cursor", cursor),
			"another currency":     q("group_by", "invoker", "currency", "EUR", "cursor", cursor),
			"another window":       q("group_by", "invoker", "cursor", cursor, "from", first.From.Add(time.Second).Format(time.RFC3339Nano)),
			"its grouping altered": q("group_by", "invoker", "cursor", edit(`"g":"invoker"`, `"g":"resource"`)),
			"a field added":        q("group_by", "invoker", "cursor", edit(`{`, `{"x":1,`)),
			"truncated":            q("group_by", "invoker", "cursor", cursor[:len(cursor)/2]),
			"not base64":           q("group_by", "invoker", "cursor", cursor[:4]+"!"+cursor[5:]),
		} {
			code, param := me.refused(query)
			require.Equal(t, []string{billing.CodeInvalidCursor, "cursor"}, []string{code, param}, name)
		}
	})

	t.Run("limit is 1 to 1,000, 100 by default", func(t *testing.T) {
		heavy := billing.CustomerID(uuid.New())
		var items []billing.RecordUsageParams
		for i := range 101 {
			items = append(items, billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, EventType: "call", Invoker: fmt.Sprintf("i-%03d", i), Amount: new(int64(1))})
		}
		record(heavy, items)
		them := readUsageAs(t, bill, heavy)

		page := them.report(q("group_by", "invoker"))
		require.Len(t, page.Rows, billing.DefaultUsagePageLimit)
		require.NotNil(t, page.NextCursor)
		rest := them.report(q("group_by", "invoker", "cursor", *page.NextCursor))
		require.Equal(t, []string{"i-100"}, []string{rest.Rows[0].Key})
		require.Nil(t, rest.NextCursor)
		require.Len(t, them.report(q("group_by", "invoker", "limit", "1000")).Rows, 101)
		require.Len(t, them.report(q("group_by", "invoker", "limit", "1")).Rows, 1)

		for _, limit := range []string{"0", "1001", "-1", "ten"} {
			code, param := them.refused(q("group_by", "invoker", "limit", limit))
			require.Equal(t, []string{billing.CodeInvalidQuery, "limit"}, []string{code, param}, limit)
		}
		code, param := them.refused(q("group_by", "tier"))
		require.Equal(t, []string{billing.CodeInvalidQuery, "group_by"}, []string{code, param})
		code, param = them.refused(q("from", "2030-02-01", "to", "2030-01-01"))
		require.Equal(t, []string{billing.CodeInvalidQuery, "from"}, []string{code, param})
	})
}
