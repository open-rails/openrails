package pagination_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type row struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// table is a list ordered by (created_at DESC, id DESC), queried the way the
// TimeID keyset query does.
type table []row

func (t table) query(afterAt *time.Time, afterID *uuid.UUID, fetch int32) []row {
	sorted := slices.Clone(t)
	slices.SortFunc(sorted, func(a, b row) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return slices.Compare(b.ID[:], a.ID[:])
	})
	var out []row
	for _, r := range sorted {
		if afterAt != nil {
			c := r.CreatedAt.Compare(*afterAt)
			if c > 0 || (c == 0 && slices.Compare(r.ID[:], afterID[:]) >= 0) {
				continue
			}
		}
		if out = append(out, r); len(out) == int(fetch) {
			break
		}
	}
	return out
}

// handler is a list route written the way a lane writes one.
func (t *table) handler(r *request.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	at, id, err := pagination.After(page.Cursor)
	if err != nil {
		var refusal *apperr.Error
		if !errors.As(err, &refusal) {
			r.InternalError("list failed", err)
			return
		}
		r.APIError(api.Coded(refusal.Code, refusal.Message).WithParam(refusal.Param))
		return
	}
	rows := t.query(at, id, pagination.Fetch(page.Limit))
	r.SuccessJSON(pagination.Cut(rows, page.Limit, func(r row) any { return pagination.TimeID{At: r.CreatedAt, ID: r.ID} }))
}

// A keyset list served over HTTP: every row exactly once across pages, in
// order, with ties on the timestamp and rows inserted between pages.
func TestKeysetListOverHTTP(t *testing.T) {
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	rows := table{}
	for i := range 23 {
		// Rows share timestamps in threes: the id breaks the tie.
		rows = append(rows, row{ID: uuid.New(), CreatedAt: base.Add(time.Duration(i/3) * time.Minute)})
	}
	routes := &router.Table{}
	router.NewMux(routes, "", nil).Handle(http.MethodGet, "/v1/things", rows.handler)
	h := routes.Handler()
	get := func(query url.Values) (int, map[string]json.RawMessage) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things?"+query.Encode(), nil))
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		return rec.Code, body
	}

	want := rows.query(nil, nil, 1000)
	var got []row
	cursor, pages := "", 0
	for {
		query := url.Values{"limit": {"5"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		status, body := get(query)
		require.Equal(t, http.StatusOK, status)
		require.Len(t, body, 2, "the envelope is exactly data and next_cursor")
		var page billing.ListPage[row]
		raw, _ := json.Marshal(body)
		require.NoError(t, json.Unmarshal(raw, &page))
		got = append(got, page.Items...)
		pages++
		if pages == 2 {
			// A row created after the first page was read is newer than the
			// cursor: it never shifts or repeats a later page.
			rows = append(rows, row{ID: uuid.New(), CreatedAt: base.Add(time.Hour)})
		}
		if page.Next == "" {
			require.Equal(t, "null", string(body["next_cursor"]))
			break
		}
		require.Len(t, page.Items, 5)
		cursor = page.Next
	}
	require.Equal(t, 5, pages)
	require.Equal(t, want, got)

	// An exact multiple of the limit ends with a full page and no cursor.
	status, body := get(url.Values{"limit": {strconv.Itoa(len(rows))}})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "null", string(body["next_cursor"]))

	// The default limit applies when none is named.
	status, body = get(url.Values{})
	require.Equal(t, http.StatusOK, status)
	var all []row
	require.NoError(t, json.Unmarshal(body["data"], &all))
	require.Len(t, all, len(rows))

	for name, tc := range map[string]struct {
		query       url.Values
		code, param string
	}{
		"limit not a number":    {url.Values{"limit": {"ten"}}, "invalid_query", "limit"},
		"limit zero":            {url.Values{"limit": {"0"}}, "invalid_query", "limit"},
		"limit over the max":    {url.Values{"limit": {strconv.Itoa(billing.MaxPageLimit + 1)}}, "invalid_query", "limit"},
		"cursor not base64":     {url.Values{"cursor": {"***"}}, "invalid_cursor", "cursor"},
		"cursor not a position": {url.Values{"cursor": {pagination.Encode(map[string]string{"x": "y"})}}, "invalid_cursor", "cursor"},
		"cursor without a key":  {url.Values{"cursor": {pagination.Encode(pagination.TimeID{})}}, "invalid_cursor", "cursor"},
	} {
		status, body := get(tc.query)
		require.Equal(t, http.StatusBadRequest, status, name)
		var refusal billing.ErrorDetails
		require.NoError(t, json.Unmarshal(body["error"], &refusal))
		require.Equal(t, tc.code, refusal.Code, name)
		require.NotNil(t, refusal.Param, name)
		require.Equal(t, tc.param, *refusal.Param, name)
	}
}

func TestLimitAndCut(t *testing.T) {
	limit, err := pagination.Limit(billing.PageRequest{})
	require.NoError(t, err)
	require.Equal(t, billing.DefaultPageLimit, limit)
	limit, err = pagination.Limit(billing.PageRequest{Limit: billing.MaxPageLimit})
	require.NoError(t, err)
	require.Equal(t, billing.MaxPageLimit, limit)
	for _, bad := range []int{-1, billing.MaxPageLimit + 1} {
		_, err = pagination.Limit(billing.PageRequest{Limit: bad})
		require.ErrorIs(t, err, pagination.ErrInvalidLimit)
		require.ErrorIs(t, err, billing.ErrInvalid)
	}
	require.EqualValues(t, 6, pagination.Fetch(5))

	position := func(n int) any { return n }
	require.Equal(t, billing.ListPage[int]{Items: []int{1, 2}}, pagination.Cut([]int{1, 2}, 2, position), "no extra row, no next page")
	page := pagination.Cut([]int{1, 2, 3}, 2, position)
	require.Equal(t, []int{1, 2}, page.Items)
	var last int
	present, err := pagination.Decode(page.Next, &last)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, 2, last, "the cursor is the last kept row's position")

	strings := pagination.Map(page, strconv.Itoa)
	require.Equal(t, billing.ListPage[string]{Items: []string{"1", "2"}, Next: page.Next}, strings)

	present, err = pagination.Decode("", &last)
	require.NoError(t, err)
	require.False(t, present)
}
