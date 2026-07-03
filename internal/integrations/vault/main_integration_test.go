//go:build integration

package vault

import (
	"testing"

	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

func TestMain(m *testing.M) { vaulttest.RunMain(m) }
