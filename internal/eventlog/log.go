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
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	shared "github.com/open-rails/helpers/eventlog"
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
	n, err := shared.Normalize(shared.Query{After: q.After, Types: q.Types, Limit: q.Limit})
	if err != nil {
		return q, apperr.Invalidf("%v", err)
	}
	return billing.EventQuery{After: n.After, Types: n.Types, Limit: n.Limit}, nil
}

func decode(raw string) (cursor, error) {
	var c cursor
	if len(raw) > 32768 {
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

// legacyCodec keeps already-issued OpenRails cursors valid during adoption.
type legacyCodec struct{}

func (legacyCodec) Encode(c shared.Cursor) string {
	epoch, _ := uuid.Parse(c.Epoch)
	return encode(cursor{Version: c.Version, Epoch: epoch, Merchant: c.Scope, Types: c.Types, Position: c.Position, Boundary: c.Boundary})
}
func (legacyCodec) Decode(raw string) (shared.Cursor, error) {
	c, err := decode(raw)
	return shared.Cursor{Version: c.Version, Source: "openrails", Scope: c.Merchant, Epoch: c.Epoch.String(), Types: c.Types, Position: c.Position, Boundary: c.Boundary}, err
}

// List uses shared cursor, retention and paging mechanics over the existing
// committed SQL journal. Domain capture/deferred flush remain unchanged.
func List(ctx context.Context, database *db.DB, merchantID billing.MerchantID, query billing.EventQuery, retention time.Duration, now time.Time) (billing.EventPage, error) {
	if merchantID.IsZero() {
		return billing.EventPage{}, apperr.Invalidf("merchant is required")
	}
	if scoped, ok := merchant.FromContext(ctx); ok && scoped != merchantID {
		return billing.EventPage{}, apperr.Invalidf("event merchant differs from the authorized scope")
	}
	log := shared.Log{Source: "openrails", Scope: merchantID.String(), Retention: retention, Now: func() time.Time { return now }, Codec: legacyCodec{}, ReadSnapshot: func(ctx context.Context, fn func(shared.Snapshot) error) error {
		return database.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(snapshot{q: gen.New(tx), mid: merchantID}) })
	}}
	p, err := log.ListEvents(ctx, shared.Query{After: query.After, Types: query.Types, Limit: query.Limit})
	if errors.Is(err, shared.ErrInvalidCursor) {
		err = invalid("event cursor belongs to another merchant or filter")
	}
	if errors.Is(err, shared.ErrExpiredCursor) {
		err = expired()
	}
	if errors.Is(err, shared.ErrInvalidQuery) {
		err = apperr.Invalidf("%v", err)
	}
	out := billing.EventPage{Events: []billing.Event{}, NextCursor: p.NextCursor, HasMore: p.HasMore, RetentionSeconds: p.RetentionSeconds}
	for _, e := range p.Events {
		out.Events = append(out.Events, billing.Event{ID: e.ID, MerchantID: merchantID, Source: e.Source, Type: e.Type, Version: e.Version, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Subject: billing.EventSubject{Type: e.Subject.Type, ID: e.Subject.ID}, Data: e.Data})
	}
	return out, err
}

type snapshot struct {
	q   *gen.Queries
	mid billing.MerchantID
}

func (s snapshot) Stream(ctx context.Context, cutoff time.Time) (shared.Stream, error) {
	head, err := s.q.GetEventLogStream(ctx, s.mid.UUID())
	if err != nil {
		return shared.Stream{}, err
	}
	out := shared.Stream{Epoch: head.Epoch.String(), Head: shared.Position{Number: head.Position, ID: head.HeadID}, Floor: shared.Position{Number: head.PrunedThrough, ID: head.PrunedID}}
	floor, err := s.q.GetExpiredEventLogBoundary(ctx, gen.GetExpiredEventLogBoundaryParams{MerchantID: s.mid.UUID(), Cutoff: cutoff})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if floor.Position > out.Floor.Number {
		out.Floor = shared.Position{Number: floor.Position, ID: floor.ID}
	}
	return out, nil
}
func (s snapshot) Boundary(ctx context.Context, pos int64) (string, error) {
	id, err := s.q.GetEventLogBoundary(ctx, gen.GetEventLogBoundaryParams{MerchantID: s.mid.UUID(), Position: pos})
	if errors.Is(err, pgx.ErrNoRows) {
		err = shared.ErrExpiredCursor
	}
	return id, err
}
func (s snapshot) Events(ctx context.Context, after int64, types []string, limit int) ([]shared.Record, error) {
	rowLimit, err := safecast.Convert[int32](limit)
	if err != nil {
		return nil, err
	}
	var rows []gen.ListEventLogRow
	if len(types) == 0 {
		rows, err = s.q.ListEventLog(ctx, gen.ListEventLogParams{MerchantID: s.mid.UUID(), AfterPosition: after, RowLimit: rowLimit})
	} else {
		var filtered []gen.ListEventLogByTypesRow
		filtered, err = s.q.ListEventLogByTypes(ctx, gen.ListEventLogByTypesParams{MerchantID: s.mid.UUID(), AfterPosition: after, RowLimit: rowLimit, EventTypes: types})
		for _, r := range filtered {
			rows = append(rows, gen.ListEventLogRow(r))
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]shared.Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, shared.Record{Position: r.Position, Event: shared.Event{ID: r.ID, Source: r.Source, Scope: s.mid.String(), Type: r.EventType, Version: int(r.Version), OccurredAt: r.OccurredAt, RecordedAt: r.RecordedAt, Subject: shared.Subject{Type: r.SubjectType, ID: r.SubjectID}, Data: r.Data}})
	}
	return out, nil
}
