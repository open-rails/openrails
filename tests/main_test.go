//go:build integration

package tests

import (
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) {
	dbtest.RunMain(m)
}
