package riverjobs

import (
	"testing"

	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/stretchr/testify/require"
)

// A provider observation retains its indexing safety lag. Its age is not a write gate.
func TestRefreshUsesObservationSafetyLag(t *testing.T) {
	require.Equal(t, providerrecovery.SafetyLag, defaultRefreshSafetyLag)
}
