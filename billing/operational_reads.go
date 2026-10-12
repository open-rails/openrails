package billing

import "time"

// AdmissionListParams selects retained admitted requests, newest admission
// first. State includes expired holds without waiting for a worker. From/To
// bound admission time as [From, To); omitted bounds cover retained history.
// Continue paging with the same customer, currency and state filters.
type AdmissionListParams struct {
	CustomerID CustomerID     `form:"customer_id"`
	Currency   string         `form:"currency"`
	State      AdmissionState `form:"state"`
	From       time.Time      `form:"from"`
	To         time.Time      `form:"to"`
	PageRequest
}

// UsageEventListParams selects recorded events, newest occurrence first.
// From/To bound occurrence time as [From, To), at most 31 days per query;
// omitted bounds select the last 31 days. Read earlier history in successive
// windows. SourceID requires Source. Continue paging with the same filters.
type UsageEventListParams struct {
	CustomerID CustomerID   `form:"customer_id"`
	Currency   string       `form:"currency"`
	EventType  string       `form:"event_type"`
	Source     string       `form:"source"`
	SourceID   string       `form:"source_id"`
	Outcome    UsageOutcome `form:"outcome"`
	From       time.Time    `form:"from"`
	To         time.Time    `form:"to"`
	PageRequest
}
