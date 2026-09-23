//go:build integration

package service

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestMerchantConfigurationApplicationReplayAndCAS(t *testing.T) {
	s, ctx := applicationService(t)
	initial, err := s.GetMerchantConfigurationState(ctx)
	require.NoError(t, err)
	name, host := "Declared name", "billing.example.test"
	params := openrails.MerchantConfigurationApplyParams{ApplicationID: "bootstrap-v1", ExpectedRevision: &initial.Revision, DisplayName: &name, APIHost: &host, Settings: &openrails.MerchantSettings{Profile: &openrails.MerchantProfileInput{DisplayName: "Original", SupportURL: "https://support.example.test"}}}
	receipt, err := s.ApplyMerchantConfiguration(ctx, params)
	require.NoError(t, err)
	require.False(t, receipt.Replayed)
	edited := "operator@example.test"
	require.NoError(t, s.SetMerchantConfiguration(ctx, MerchantConfiguration{AlertEmail: &edited}))
	state, err := s.GetMerchantConfigurationState(ctx)
	require.NoError(t, err)
	require.NotEqual(t, receipt.Revision, state.Revision)
	replay, err := s.ApplyMerchantConfiguration(ctx, params)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, receipt.Revision, replay.Revision)
	state, err = s.GetMerchantConfigurationState(ctx)
	require.NoError(t, err)
	require.Equal(t, edited, *state.Settings.AlertEmail)
	stale := params
	stale.ApplicationID = "bootstrap-v2"
	_, err = s.ApplyMerchantConfiguration(ctx, stale)
	require.ErrorContains(t, err, "changed")
	changed := params
	other := "Conflicting name"
	changed.DisplayName = &other
	_, err = s.ApplyMerchantConfiguration(ctx, changed)
	require.ErrorContains(t, err, "different content")
	patch := openrails.MerchantConfigurationApplyParams{ApplicationID: "support-v2", ExpectedRevision: &state.Revision, Settings: &openrails.MerchantSettings{Profile: &openrails.MerchantProfileInput{SupportURL: "https://new.example.test"}}}
	_, err = s.ApplyMerchantConfiguration(ctx, patch)
	require.NoError(t, err)
	state, err = s.GetMerchantConfigurationState(ctx)
	require.NoError(t, err)
	require.Equal(t, name, state.DisplayName)
	require.Equal(t, host, state.APIHost)
	require.Equal(t, "Original", state.Settings.Profile.DisplayName)
	require.Equal(t, edited, *state.Settings.AlertEmail)
	// Same application ID in a different merchant cannot read the first receipt.
	otherService, otherCtx := applicationService(t)
	_, err = otherService.ApplyMerchantConfiguration(otherCtx, params)
	require.Error(t, err)
}

func TestMerchantConfigurationApplicationWaitsForOrdinaryWriter(t *testing.T) {
	s, ctx := applicationService(t)
	state, err := s.GetMerchantConfigurationState(ctx)
	require.NoError(t, err)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var writerPID int
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
				return err
			}
			database := s.rt.DB.NewWithPgxTx(tx)
			scoped := &Service{rt: &app.Runtime{DB: database}}
			email := "concurrent@example.test"
			if err := scoped.SetMerchantConfiguration(ctx, MerchantConfiguration{AlertEmail: &email}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	applied := make(chan error, 1)
	go func() {
		name := "stale update"
		_, err := s.ApplyMerchantConfiguration(ctx, openrails.MerchantConfigurationApplyParams{ApplicationID: uuid.NewString(), ExpectedRevision: &state.Revision, DisplayName: &name})
		applied <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := dbtest.SharedSuperuserPGXPool(t).QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))", writerPID).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "application must wait behind the uncommitted ordinary writer")
	unblock()
	require.NoError(t, <-writerDone)
	require.ErrorContains(t, <-applied, "changed")
	var count int
	require.NoError(t, s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.rt.DB.Qx(ctx).QueryRow(ctx, "SELECT count(*) FROM openrails.merchant_configuration_applications WHERE merchant_id=$1", mid.UUID()).Scan(&count)
	}))
	require.Zero(t, count)
}
