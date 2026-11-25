package service

import (
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// inspectionCursor keeps the time window fixed while traversing a keyset.
// Filters remain explicit request parameters, as on other merchant lists.
type inspectionCursor struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Key  string    `json:"key"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type inspectionPage struct {
	limit    int
	from, to time.Time
	after    *inspectionCursor
}

func invalidInspectionQuery(param, message string) error {
	return apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, message).WithParam(param)
}

func readInspectionPage(kind string, page billing.PageRequest, from, to, defaultFrom, now time.Time) (inspectionPage, error) {
	out := inspectionPage{from: from.UTC(), to: to.UTC()}
	var err error
	if out.limit, err = pagination.Limit(page); err != nil {
		return out, err
	}
	var after inspectionCursor
	present, err := pagination.Decode(page.Cursor, &after)
	if err != nil {
		return out, err
	}
	if present {
		if after.Kind != kind || after.At.IsZero() || after.Key == "" || after.From.IsZero() || after.To.IsZero() ||
			(!from.IsZero() && !from.Equal(after.From)) || (!to.IsZero() && !to.Equal(after.To)) {
			return out, pagination.ErrInvalidCursor
		}
		out.from, out.to, out.after = after.From.UTC(), after.To.UTC(), &after
	}
	if out.to.IsZero() {
		out.to = now.UTC().Add(time.Microsecond)
	}
	if out.from.IsZero() {
		out.from = defaultFrom.UTC()
	}
	if !out.from.Before(out.to) {
		return out, invalidInspectionQuery("from", "from must be before to")
	}
	if out.after != nil && (out.after.At.Before(out.from) || !out.after.At.Before(out.to)) {
		return out, pagination.ErrInvalidCursor
	}
	return out, nil
}

func (p inspectionPage) position(kind string, at time.Time, key string) inspectionCursor {
	return inspectionCursor{Kind: kind, At: at, Key: key, From: p.from, To: p.to}
}

func optionalInspectionText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func inspectionCurrency(currency string) (*string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return nil, nil
	}
	normalized, err := requireCurrency(currency)
	return &normalized, err
}

func retainedFrom(from, oldest time.Time) time.Time {
	if from.Before(oldest) {
		return oldest
	}
	return from
}
