// Package eventlog exposes committed merchant facts without managing readers.
package eventlog

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

const DefaultRetention = retention.EventLog

type cursor struct {
	Version  int       `json:"v"`
	Epoch    uuid.UUID `json:"epoch"`
	Merchant string    `json:"merchant"`
	Types    []string  `json:"types,omitempty"`
	Position int64     `json:"position"`
	Boundary string    `json:"boundary,omitempty"`
}

type stream struct {
	Epoch    uuid.UUID
	Position int64
	HeadID   string
	Floor    int64
	FloorID  string
}

func invalid(message string) error {
	return apperr.New(http.StatusBadRequest, billing.CodeInvalidCursor, message).WithParam("after")
}
func expired() error {
	return apperr.New(http.StatusGone, billing.CodeEventCursorExpired, "event cursor is no longer retained; resynchronize current state").WithParam("after")
}

func normalize(q billing.EventQuery) (billing.EventQuery, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > 500 {
		return q, apperr.Invalidf("limit must be between 1 and 500").WithParam("limit")
	}
	if len(q.Types) > 32 {
		return q, apperr.Invalidf("at most 32 event types may be selected").WithParam("type")
	}
	q.Types = slices.Clone(q.Types)
	for i, value := range q.Types {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 {
			return q, apperr.Invalidf("invalid event type").WithParam("type")
		}
		q.Types[i] = value
	}
	sort.Strings(q.Types)
	q.Types = slices.Compact(q.Types)
	return q, nil
}

func decode(raw string) (cursor, error) {
	var c cursor
	if len(raw) > 8192 {
		return c, invalid("invalid event cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, invalid("invalid event cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.Version != 1 || c.Epoch == uuid.Nil || c.Position < 0 {
		return c, invalid("invalid event cursor")
	}
	return c, nil
}
func encode(c cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// List uses one read-only snapshot for the head, retention floor and page.
// A transaction that has not committed cannot advance the returned cursor.
func List(ctx context.Context, database *db.DB, merchantID billing.MerchantID, query billing.EventQuery, retention time.Duration, now time.Time) (billing.EventPage, error) {
	page := billing.EventPage{Events: []billing.Event{}, RetentionSeconds: int64(retention / time.Second)}
	if merchantID.IsZero() {
		return page, apperr.Invalidf("merchant is required")
	}
	if scoped, ok := merchant.FromContext(ctx); ok && scoped != merchantID {
		return page, apperr.Invalidf("event merchant differs from the authorized scope")
	}
	q, err := normalize(query)
	if err != nil {
		return page, err
	}
	rowLimit, err := safecast.Convert[int32](q.Limit + 1)
	if err != nil {
		return page, err
	}
	var prior cursor
	if q.After != "" {
		prior, err = decode(q.After)
		if err != nil {
			return page, err
		}
		if prior.Merchant != merchantID.String() || !slices.Equal(prior.Types, q.Types) {
			return page, invalid("event cursor belongs to another merchant or filter")
		}
	}
	err = database.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queries := gen.New(tx)
		head, err := queries.GetEventLogStream(ctx, merchantID.UUID())
		if err != nil {
			return err
		}
		s := stream{Epoch: head.Epoch, Position: head.Position, HeadID: head.HeadID, Floor: head.PrunedThrough, FloorID: head.PrunedID}
		floor, err := queries.GetExpiredEventLogBoundary(ctx, gen.GetExpiredEventLogBoundaryParams{MerchantID: merchantID.UUID(), Cutoff: now.Add(-retention)})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if floor.Position > s.Floor {
			s.Floor, s.FloorID = floor.Position, floor.ID
		}
		after := s.Floor
		if q.After != "" {
			if prior.Epoch != s.Epoch || prior.Position > s.Position || prior.Position < s.Floor {
				return expired()
			}
			after = prior.Position
			boundary := ""
			if after == s.Floor {
				boundary = s.FloorID
			} else if after > 0 {
				var err error
				boundary, err = queries.GetEventLogBoundary(ctx, gen.GetEventLogBoundaryParams{MerchantID: merchantID.UUID(), Position: after})
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return expired()
					}
					return err
				}
			}
			if boundary != prior.Boundary {
				return expired()
			}
		}
		var rows []gen.ListEventLogRow
		if len(q.Types) == 0 {
			rows, err = queries.ListEventLog(ctx, gen.ListEventLogParams{MerchantID: merchantID.UUID(), AfterPosition: after, RowLimit: rowLimit})
		} else {
			var filtered []gen.ListEventLogByTypesRow
			filtered, err = queries.ListEventLogByTypes(ctx, gen.ListEventLogByTypesParams{MerchantID: merchantID.UUID(), AfterPosition: after, EventTypes: q.Types, RowLimit: rowLimit})
			for _, row := range filtered {
				rows = append(rows, gen.ListEventLogRow(row))
			}
		}
		if err != nil {
			return err
		}

		position, boundary := s.Position, s.HeadID
		for _, row := range rows {
			if len(page.Events) == q.Limit {
				page.HasMore = true
				break
			}
			event := billing.Event{ID: row.ID, MerchantID: merchantID, Source: row.Source, Type: row.EventType, Version: int(row.Version), OccurredAt: row.OccurredAt, Subject: billing.EventSubject{Type: row.SubjectType, ID: row.SubjectID}, Data: row.Data}
			page.Events = append(page.Events, event)
			position, boundary = row.Position, row.ID
		}
		if !page.HasMore {
			position, boundary = s.Position, s.HeadID
		}
		page.NextCursor = encode(cursor{Version: 1, Epoch: s.Epoch, Merchant: merchantID.String(), Types: q.Types, Position: position, Boundary: boundary})
		return nil
	})
	return page, err
}
