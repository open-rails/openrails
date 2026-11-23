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
