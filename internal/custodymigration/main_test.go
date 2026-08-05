//go:build integration

package custodymigration_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }
