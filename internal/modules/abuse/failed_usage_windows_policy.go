package abuse

import "time"

// WastedWindow is one configured failed-usage window: at most Limit of failed
// usage per Window, in Currency (the event's when empty). A customer's grace
// windows forgive up to Limit; a delegated invoker's cutoff windows refuse its
// admissions once reached. They are counted in PostgreSQL with the usage
// events that fill them (money.FailedUsageWindow).
type WastedWindow struct {
	Key      string
	Window   time.Duration
	Limit    int64
	Currency string
}
