package billing

import "errors"

// ErrMigrationsRequired means openrails.New, with
// Config.Database.SkipMigrations, found a database openrails.Migrate has not
// prepared for this build: a migration is missing, or was applied with other
// content. The validation failure is preserved.
var ErrMigrationsRequired = errors.New("openrails: database migrations are required")
