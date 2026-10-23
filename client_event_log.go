package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/helpers/eventlog"
	"github.com/open-rails/openrails/billing"
)

// ListEvents reads retained facts of the handle's merchant. Reading starts no
// worker and changes no delivery state. Persist NextCursor after processing.
func (c *Client) ListEvents(ctx context.Context, query billing.EventQuery, options ...RequestOption) (*billing.EventPage, error) {
	values := url.Values{}
	if query.After != "" {
		values.Set("after", query.After)
	}
	if query.Limit != 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	for _, kind := range query.Types {
		values.Add("type", kind)
	}
	var out billing.EventPage
	if err := c.do(ctx, http.MethodGet, "/v1/app/events?"+values.Encode(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// EventReader adapts the merchant client to the shared consumer interface. The
// same reader works for Engine.Merchant and remote HTTP clients; credentials,
// authorization and merchant binding remain in the client transport.
func (c *Client) EventReader() eventlog.Reader {
	return eventlog.ReaderFunc(func(ctx context.Context, q eventlog.Query) (eventlog.Page, error) {
		normalized, err := eventlog.Normalize(q)
		if err != nil {
			return eventlog.Page{}, err
		}
		p, err := c.ListEvents(ctx, billing.EventQuery{After: normalized.After, Types: normalized.Types, Limit: normalized.Limit})
		if err != nil {
			var status *billing.StatusError
			if errors.As(err, &status) {
				switch status.Code {
				case billing.CodeInvalidCursor:
					return eventlog.Page{}, errors.Join(eventlog.ErrInvalidCursor, err)
				case billing.CodeEventCursorExpired:
					return eventlog.Page{}, errors.Join(eventlog.ErrExpiredCursor, err)
				}
			}
			return eventlog.Page{}, err
		}
		out := eventlog.Page{Events: []eventlog.Event{}, NextCursor: p.NextCursor, HasMore: p.HasMore, RetentionSeconds: p.RetentionSeconds}
		for _, e := range p.Events {
			out.Events = append(out.Events, eventlog.Event{ID: e.ID, Source: e.Source, Scope: e.MerchantID.String(), Type: e.Type, Version: e.Version, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Subject: eventlog.Subject{Type: e.Subject.Type, ID: e.Subject.ID}, Data: e.Data})
		}
		return out, nil
	})
}
