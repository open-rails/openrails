package merchants

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCredentialFloorRefusesBackendLagAndRecovers(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "uncached"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			ctx, owner := t.Context(), merchant.ID(uuid.New())
			backend := NewMemorySecretStore()
			first, err := backend.Put(ctx, owner, "fixture/api_key", "synthetic-first")
			require.NoError(t, err)
			reader := backend
			if cached {
				reader = NewCachedSecretStore(backend, time.Hour)
			}
			_, err = reader.Get(ctx, owner, first.Name)
			require.NoError(t, err)
			ref := SecretRef{Name: first.Name, MinVersion: first.Version + 1}
			value, err := ReadSecretRef(ctx, reader, owner, ref)
			require.Error(t, err, "a current backend read does not prove its version meets the recorded floor")
			require.Empty(t, value.Value)
			if versioned, ok := reader.(VersionedSecretReader); ok {
				_, err = versioned.GetAtLeastVersion(ctx, owner, ref.Name, ref.MinVersion)
				require.Error(t, err, "the cache must also honor its direct versioned-read contract")
			}
			second, err := backend.Put(ctx, owner, first.Name, "synthetic-second")
			require.NoError(t, err)
			value, err = ReadSecretRef(ctx, reader, owner, ref)
			require.NoError(t, err)
			require.Equal(t, second.Version, value.Version)
			require.Equal(t, second.Value, value.Value)
		})
	}
}
