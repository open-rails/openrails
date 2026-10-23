package billing

import (
	"encoding/json"
	"time"
)

const CodeEventCursorExpired = "event_cursor_expired"

// EventQuery reads committed facts after a caller-owned cursor. Types is fixed
// for that cursor; an empty cursor starts at the oldest retained fact.
type EventQuery struct {
	After string
	Limit int
	Types []string
}

type EventSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Event contains committed facts, not delivery instructions. IDs retain their
// existing hev_ or ntf_ identity so readers can deduplicate during migration.
type Event struct {
	ID         string          `json:"id"`
	MerchantID MerchantID      `json:"merchant_id"`
	Source     string          `json:"source"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	RecordedAt time.Time       `json:"recorded_at"`
	OccurredAt time.Time       `json:"occurred_at"`
	Subject    EventSubject    `json:"subject"`
	Data       json.RawMessage `json:"data"`
}

// EventPage changes no source state. Save NextCursor only after the returned
// events have been processed. Even an empty page has a continuation cursor.
type EventPage struct {
	Events           []Event `json:"events"`
	NextCursor       string  `json:"next_cursor"`
	HasMore          bool    `json:"has_more"`
	RetentionSeconds int64   `json:"retention_seconds"`
}
