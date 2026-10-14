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
	_, ok := UniqueViolation(err)
	return ok
}

// UniqueViolation is the constraint a unique violation names ("" when the
// driver names none).
func UniqueViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
