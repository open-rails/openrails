package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// IsNotFound reports whether err is pgx.ErrNoRows or sql.ErrNoRows (which
// internal not-found sentinels return).
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows)
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505), by code rather than by message text.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
