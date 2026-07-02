package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

// IsNotFound reports whether err is a no-rows error from either driver era:
// pgx.ErrNoRows (sqlc/pgx paths, #334) or sql.ErrNoRows (bun-era paths and
// internal not-found sentinels). Callers use this instead of testing a single
// driver's sentinel, so not-found handling stays correct through the bun ->
// sqlc migration regardless of which side produced the error.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows)
}
