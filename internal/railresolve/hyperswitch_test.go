package railresolve

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type hsSecretReader struct {
	secret merchants.Secret
	err    error
	owner  merchant.ID
	name   string
	floor  int
}

func (s *hsSecretReader) Get(_ context.Context, owner merchant.ID, name string) (merchants.Secret, error) {
	s.owner, s.name = owner, name
	return s.secret, s.err
}
func (s *hsSecretReader) GetVersion(ctx context.Context, owner merchant.ID, name string, floor int) (merchants.Secret, error) {
	s.floor = floor
	return s.Get(ctx, owner, name)
}

func TestHyperSwitchCredentialResolution(t *testing.T) {
	owner := merchant.ID(uuid.New())
	base := gen.OpenrailsCustodian{ID: uuid.New(), MerchantID: owner.UUID(), Kind: "hyperswitch", Environment: "test", AccountID: "merchant_A", Settings: []byte(`{"public_api_key":"public_A","profile_id":"profile_A"}`), CredentialVersions: []byte(`{"api_key":2}`)}
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: "https://owned-custody.example"}}
	for _, tc := range []struct {
		name      string
		mutate    func(*gen.OpenrailsCustodian)
		version   int
		secret    string
		err       error
		wantError bool
		floor     int
	}{
		{name: "current key", version: 2, secret: "private", floor: 2},
		{name: "archived owned account still addressable", mutate: func(r *gen.OpenrailsCustodian) { r.Archived = true }, version: 2, secret: "private", floor: 2},
		{name: "unrotated declaration", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(`{}`) }, version: 1, secret: "private"},
		{name: "missing document", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = nil }, wantError: true},
		{name: "malformed floor", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(`{"api_key":"2"}`) }, wantError: true},
		{name: "null document", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(`null`) }, wantError: true},
		{name: "null declared floor", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(`{"api_key":null}`) }, wantError: true},
		{name: "null settings", mutate: func(r *gen.OpenrailsCustodian) { r.Settings = []byte(`null`) }, wantError: true},
		{name: "negative floor", mutate: func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(`{"api_key":-1}`) }, wantError: true},
		{name: "stale returned key", version: 1, secret: "private", wantError: true, floor: 2},
		{name: "newer unpublished key", version: 3, secret: "private", wantError: true, floor: 2},
		{name: "empty key", version: 2, wantError: true, floor: 2},
		{name: "missing secret", err: merchants.ErrSecretNotFound, wantError: true, floor: 2},
		{name: "foreign owner", mutate: func(r *gen.OpenrailsCustodian) { r.MerchantID = uuid.New() }, wantError: true},
		{name: "wrong environment", mutate: func(r *gen.OpenrailsCustodian) { r.Environment = "live" }, wantError: true},
		{name: "wrong kind", mutate: func(r *gen.OpenrailsCustodian) { r.Kind = "basis_theory" }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base
			if tc.mutate != nil {
				tc.mutate(&row)
			}
			secrets := &hsSecretReader{secret: merchants.Secret{Value: tc.secret, Version: tc.version}, err: tc.err}
			client, err := HyperSwitchClient(t.Context(), cfg, secrets, owner, row)
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, client)
			} else {
				require.NoError(t, err)
				require.NotNil(t, client)
				require.Equal(t, owner, secrets.owner)
				require.Equal(t, "custodians/hyperswitch/test/merchant_A/api_key", secrets.name)
			}
			require.Equal(t, tc.floor, secrets.floor)
		})
	}
	_, err := HyperSwitchClient(t.Context(), cfg, nil, owner, base)
	require.ErrorIs(t, err, hyperswitch.ErrBinding)
}
