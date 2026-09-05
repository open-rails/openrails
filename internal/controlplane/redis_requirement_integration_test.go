//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// noopEmailSender satisfies authcore.EmailSender without sending anything —
// enough to let hosted posture's Registration.Verification=Required
// construction check pass; this file's tests never exercise a route.
type noopEmailSender struct{}

func (noopEmailSender) SendVerification(context.Context, string, string, authcore.VerificationMessage) error {
	return nil
}
func (noopEmailSender) SendPasswordResetLink(context.Context, string, string, string) error {
	return nil
}
func (noopEmailSender) SendAccountRegistrationInvite(context.Context, string, string) error {
	return nil
}
func (noopEmailSender) SendLoginCode(context.Context, string, string, string) error { return nil }
func (noopEmailSender) SendWelcome(context.Context, string, string) error           { return nil }

// stagingControlPlaneConfig is newTestControlPlane's cfg with a non-development
// Environment (#753): authhttp.NewServer's own construction-time validate()
// requires a Redis-backed ephemeral store whenever Environment is not dev-like
// (embedded.IsDevEnvironment) — "staging" here is what actually exercises that
// gate ("dev", used everywhere else in this file, never does).
// Auth.MintDisabled=true sidesteps signing-key discovery (no keys.json exists
// in this test environment, which outside development would otherwise be a
// separate, unrelated construction failure — irrelevant to the Redis gate
// under test here).
func stagingControlPlaneConfig() *config.Config {
	return &config.Config{
		Env: "staging",
		Auth: &config.AuthConfig{
			Issuer:       "https://openrails-staging.test",
			MintDisabled: true,
			DirectPeerIP: true,
		},
	}
}

// TestNew_HostedStagingWithRedis_Succeeds proves #753's fix: New now accepts
// WithRedis, wiring the caller's Redis client into AuthKit's engine as its
// ephemeral store (authcore.WithRedis) — which authhttp.NewServer (authkit
// v0.79.0, #210) also reuses for its own HTTP-layer rate limiter/state caches.
// A hosted control plane in a non-development environment must boot when
// Redis is supplied.
func TestNew_HostedStagingWithRedis_Succeeds(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	rdb, _ := dbtest.SharedRedisClient(t)

	cp, err := New(ctx, stagingControlPlaneConfig(), pool,
		WithHostedPosture(),
		WithEmailSender(noopEmailSender{}),
		WithRedis(rdb),
	)
	require.NoError(t, err, "hosted control plane in a non-development environment must boot when Redis is wired")
	require.NotNil(t, cp)
}

// TestNew_HostedStagingWithoutRedis_FailsNamingRedis is the mirror: before
// #753, internal/controlplane.New never wired Redis/an ephemeral store into
// authhttp.NewServer at all, so ANY hosted construction with a
// production-like Environment string hard-failed unconditionally. With no
// WithRedis option supplied, New must still fail — but the error must clearly
// name the Redis/ephemeral-store requirement so an operator can act on it.
func TestNew_HostedStagingWithoutRedis_FailsNamingRedis(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)

	cp, err := New(ctx, stagingControlPlaneConfig(), pool,
		WithHostedPosture(),
		WithEmailSender(noopEmailSender{}),
	)
	require.Error(t, err, "a non-development control plane with no Redis wired must fail, not silently boot unprotected")
	require.Nil(t, cp)
	require.Contains(t, strings.ToLower(err.Error()), "redis",
		"the failure must name the Redis/ephemeral-store requirement: %v", err)
}

func (noopEmailSender) SendContactChanged(context.Context, string, string, authcore.ContactChange) error {
	return nil
}

func (noopEmailSender) SendDeviceKeyEnrolled(context.Context, string, string, authcore.DeviceKeyNotice) error {
	return nil
}

func TestProductionClientIPPosture(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	for _, tc := range []struct {
		name    string
		direct  bool
		proxies []string
		opts    []Option
		wantErr string
	}{
		{name: "undeclared", wantErr: "explicit client-IP posture"},
		{name: "configured direct peer", direct: true},
		{name: "host direct peer", opts: []Option{WithDirectPeerIP()}},
		{name: "configured proxies", proxies: []string{"192.0.2.0/24"}},
		{name: "host proxies", opts: []Option{WithTrustedProxies([]string{"192.0.2.0/24"})}},
		{name: "conflicting posture", direct: true, proxies: []string{"192.0.2.0/24"}, wantErr: "conflicts with trusted_proxies"},
		{name: "invalid proxy", proxies: []string{"not-a-cidr"}, wantErr: "invalid trusted proxy CIDR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stagingControlPlaneConfig()
			cfg.Auth.DirectPeerIP = tc.direct
			cfg.TrustedProxies = tc.proxies
			opts := append([]Option{WithHostedPosture(), WithEmailSender(noopEmailSender{}), WithRedis(rdb)}, tc.opts...)
			cp, err := New(ctx, cfg, pool, opts...)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, cp)
		})
	}
}
