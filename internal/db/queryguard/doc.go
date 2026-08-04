// Package queryguard holds schema-derived guards over the generated sqlc query
// text. It has no runtime code: the guards are tests, and they run in the
// ordinary suite (no database, no cgo) so they cannot be skipped silently the
// way the sqlaudit gate is when SQLC_DATABASE_URL is unset.
package queryguard
