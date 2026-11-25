package billing

import "errors"

// ErrMigrationsRequired means a one-off command found an event store
// openrails.MigrateClickHouse has not prepared for this build: a migration is
// missing, or was applied with other content. The validation failure is
// preserved.
var ErrMigrationsRequired = errors.New("openrails: database migrations are required")
