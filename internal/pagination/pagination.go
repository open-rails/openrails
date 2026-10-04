// Package pagination is the one keyset-pagination helper behind the list
// envelope (billing.ListPage): the request's limit and cursor, the opaque
// cursor codec, and the cut from a fetched row set to a page.
//
// A list handler reads the request with Request.Page, decodes the cursor into
// its position type with Decode, runs its sqlc query for Fetch(limit) rows
// ordered by that position, and answers Cut(rows, limit, position).
package pagination

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrInvalidCursor is a cursor this list did not issue (400 invalid_cursor).
var ErrInvalidCursor = apperr.New(http.StatusBadRequest, billing.CodeInvalidCursor, "the cursor is not one this list issued").WithParam("cursor")

// ErrInvalidLimit is a limit outside 1..billing.MaxPageLimit (400 invalid_query).
var ErrInvalidLimit = apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "limit is invalid").WithParam("limit")

// Limit is the page size a request asks for: billing.DefaultPageLimit when it
// names none, ErrInvalidLimit outside 1..billing.MaxPageLimit.
func Limit(page billing.PageRequest) (int, error) {
	switch {
	case page.Limit == 0:
		return billing.DefaultPageLimit, nil
	case page.Limit < 1 || page.Limit > billing.MaxPageLimit:
		return 0, ErrInvalidLimit
	}
	return page.Limit, nil
}

// Fetch is the row count a keyset query asks for: one more than the page, so
// Cut can tell whether another page follows without a count query. A limit
// outside 1..billing.MaxPageLimit (Limit refuses it) asks for a full page.
func Fetch(limit int) int32 {
	if limit < 1 || limit > billing.MaxPageLimit {
		limit = billing.MaxPageLimit
	}
	return int32(limit) + 1 // #nosec G115 -- bounded to 1..MaxPageLimit above
}

// Encode makes an opaque cursor from a keyset position: the ordering key of
// the last row of a page. Clients pass it back verbatim.
func Encode(position any) string {
	raw, err := json.Marshal(position)
	if err != nil {
		panic("pagination: unencodable cursor position: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Decode reads a cursor made by Encode into position and reports whether the
// request carried one. An empty cursor is the first page; anything else that
// is not a cursor is ErrInvalidCursor.
func Decode(cursor string, position any) (bool, error) {
	if cursor == "" {
		return false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return false, ErrInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(position); err != nil {
		return false, ErrInvalidCursor
	}
	return true, nil
}

// Cut turns the rows of a query for Fetch(limit) rows into the page: the
// first limit rows, and the cursor of the next page when a further row came
// back. position is the ordering key of a row.
func Cut[T any](rows []T, limit int, position func(T) any) billing.ListPage[T] {
	if len(rows) <= limit {
		return billing.ListPage[T]{Items: rows}
	}
	return billing.ListPage[T]{Items: rows[:limit], Next: Encode(position(rows[limit-1]))}
}

// Map converts a page's items, keeping its cursor.
func Map[T, U any](page billing.ListPage[T], convert func(T) U) billing.ListPage[U] {
	out := billing.ListPage[U]{Items: make([]U, len(page.Items)), Next: page.Next}
	for i, item := range page.Items {
		out.Items[i] = convert(item)
	}
	return out
}

// TimeID is the usual keyset position: a list ordered by (time, id). Its
// query is
//
//	WHERE (sqlc.narg(after_at)::timestamptz IS NULL
//	       OR (created_at, id) < (sqlc.narg(after_at), sqlc.narg(after_id)))
//	ORDER BY created_at DESC, id DESC
//	LIMIT @fetch
//
// with After's values as the two nullable arguments.
type TimeID struct {
	At time.Time `json:"t"`
	ID uuid.UUID `json:"i"`
}

// After reads a TimeID cursor as a keyset query's nullable arguments: both
// nil on the first page.
func After(cursor string) (at *time.Time, id *uuid.UUID, err error) {
	var position TimeID
	present, err := Decode(cursor, &position)
	if err != nil || !present {
		return nil, nil, err
	}
	if position.At.IsZero() || position.ID == uuid.Nil {
		return nil, nil, ErrInvalidCursor
	}
	return &position.At, &position.ID, nil
}
