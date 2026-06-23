//go:build integration

package ratelimit_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

// TestMain tears down the shared integration Postgres/Redis after the package's
// tests so the per-run database and container do not leak (dbtest.RunMain).
func TestMain(m *testing.M) { dbtest.RunMain(m) }
