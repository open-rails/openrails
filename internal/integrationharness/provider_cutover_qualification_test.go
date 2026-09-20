//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type cutoverRenewal struct {
	renewed bool
	err     error
}
type cutoverHeartbeatStore struct {
	*intents.Store
	beats    chan cutoverRenewal
	attempts chan struct{}
}

func (s *cutoverHeartbeatStore) RenewClaim(ctx context.Context, id uuid.UUID, now, until time.Time) (bool, error) {
	if s.attempts != nil {
		s.attempts <- struct{}{}
	}
	renewed, err := s.Store.RenewClaim(ctx, id, now, until)
	s.beats <- cutoverRenewal{renewed, err}
	return renewed, err
}

type cutoverScopedClients map[uuid.UUID]*nmi.NMIClient

func (clients cutoverScopedClients) ResolveNMIClient(_ context.Context, _ uuid.UUID, id *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if id == nil {
		return nil, false, nil
	}
	client := clients[*id]
	return client, client != nil, nil
}

func TestNMIProviderCutoverQualification(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	var prior bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM openrails.destructive_action_switch`).Scan(&prior))
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.WithoutCancel(ctx), `UPDATE openrails.destructive_action_switch SET enabled=$1`, prior)
		require.NoError(t, err)
	})
	_, err := h.Pool().Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	rt := s.App().Runtime
	rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("qualified-cutover-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	set := func(id uuid.UUID, record *providerqualification.Record) error {
		return operator.SetProviderCutoverQualification(ctx, s.App(), owner.MerchantID, id, record)
	}
	record := func(id uuid.UUID) *providerqualification.Record {
		return &providerqualification.Record{PSPID: id, Environment: "test", Contract: providerqualification.NMIContract, EvidenceRef: "local-qualified-fixture"}
	}
	seed := func(t *testing.T, mode string) cutoverHTTPFixture {
		return seedCutoverHTTP(t, h, s, g, mode, owner.MerchantID)
	}

	t.Run("admission_and_record_binding", func(t *testing.T) {
		p := seed(t, "success")
		for _, id := range []uuid.UUID{p.Source, p.Target} {
			require.NoError(t, set(id, nil))
			path := s.BaseURL + "/v1/merchant/subscriptions/" + openrails.SubscriptionID(p.Sub).String() + "/provider-cutover/preview"
			status, raw := requestJSON(t, http.MethodPost, path, owner.APIKey, p.Req)
			require.Equal(t, http.StatusConflict, status)
			requireCutoverError(t, raw, "provider_cutover_unqualified")
			_, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), uuid.NewString(), p.Req)
			require.Error(t, err)
			require.NoError(t, set(id, record(id)))
		}
		for _, field := range []string{"psp", "environment", "contract", "reference"} {
			bad := record(p.Target)
			switch field {
			case "psp":
				bad.PSPID = p.Source
			case "environment":
				bad.Environment = "live"
			case "contract":
				bad.Contract = "unsupported"
			case "reference":
				bad.EvidenceRef = "https://example.com/?secret=not-allowed"
			}
			require.ErrorIs(t, set(p.Target, bad), providerqualification.ErrInvalid)
			encoded, err := json.Marshal(map[string]any{"settings": map[string]any{"nmi_cutover_qualification": bad}})
			require.NoError(t, err)
			_, err = providerqualification.Current(gen.OpenrailsPsp{ID: p.Target, Rail: "nmi", Environment: "test", Evidence: encoded})
			require.ErrorIs(t, err, providerqualification.ErrInvalid, "manifest ingestion shares the same validator")
		}
		require.ErrorIs(t, set(uuid.New(), nil), providerqualification.ErrNotFound)
		var operations int
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&operations))
		require.Zero(t, operations)
	})

	t.Run("fresh_create_marker_refused_before_http_is_terminal", func(t *testing.T) {
		p := seed(t, "success")
		key := uuid.NewString()
		tx, err := h.Pool().Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
		_, err = tx.Exec(ctx, `SELECT id FROM openrails.psps WHERE id=$1 FOR NO KEY UPDATE`, p.Target)
		require.NoError(t, err)
		type answer struct {
			result *openrails.ProviderCutover
			err    error
		}
		done := make(chan answer, 1)
		go func() {
			result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			done <- answer{result, err}
		}()
		require.Eventually(t, func() bool {
			var marked bool
			err := h.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.rail_intents WHERE subscription_id=$1 AND result_evidence->>'create_submitted'='true')`, p.Sub).Scan(&marked)
			return err == nil && marked
		}, 5*time.Second, 5*time.Millisecond)
		_, err = tx.Exec(ctx, `UPDATE openrails.psps SET evidence=evidence #- '{settings,nmi_cutover_qualification}' WHERE id=$1`, p.Target)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		out := <-done
		require.NoError(t, out.err)
		require.Equal(t, "failed_terminal", out.result.Status)
		require.Equal(t, "not_executed", out.result.Stage)
		g.mu.Lock()
		creates := g.Accounts[p.TargetKey].Creates
		g.mu.Unlock()
		require.Zero(t, creates)
		require.NoError(t, set(p.Target, record(p.Target)))
		replay, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "failed_terminal", replay.Status)
		completed, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), uuid.NewString(), p.Req)
		require.NoError(t, err)
		require.Equal(t, "succeeded", completed.Status)
	})

	t.Run("existing_unknown_create_marker_is_never_erased", func(t *testing.T) {
		p := seed(t, "lost_create")
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		require.NoError(t, set(p.Target, nil))
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		g.mu.Lock()
		creates := g.Accounts[p.TargetKey].Creates
		g.mu.Unlock()
		require.Equal(t, 1, creates)
	})

	t.Run("source_revocation_allows_qualified_target_compensation", func(t *testing.T) {
		p := seed(t, "source_dark")
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		require.NoError(t, set(p.Source, nil))
		g.mu.Lock()
		g.Accounts[p.SourceKey].Mode = ""
		g.mu.Unlock()
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "pending", result.Status)
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
			_, err := rt.IntentRunner().Resolve(cctx, result.ID, intents.Resolution{Step: "target", Abandon: true, Actor: "fixture", Reason: "source qualification revoked"})
			return err
		}))
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "abandoned", result.Stage)
		g.mu.Lock()
		sourceDeletes, targetDeletes := g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Deletes
		g.mu.Unlock()
		require.Zero(t, sourceDeletes)
		require.Equal(t, 1, targetDeletes)
	})
	t.Run("revocation_preserves_read_only_activation_recovery", func(t *testing.T) {
		p := seed(t, "lost_activation")
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		require.NoError(t, set(p.Source, nil))
		require.NoError(t, set(p.Target, nil))
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "succeeded", result.Status)
		g.mu.Lock()
		sourceDeletes, activations := g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Activations
		g.mu.Unlock()
		require.Equal(t, 1, sourceDeletes)
		require.Equal(t, 1, activations)
	})
	t.Run("revocation_serializes_with_one_connection_pinned_write", func(t *testing.T) {
		p := seed(t, "success")
		makeDB := func() *db.DB {
			cfg, err := pgxpool.ParseConfig(h.DSN)
			require.NoError(t, err)
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			database, err := db.NewWithPGXPool(pool, "openrails")
			require.NoError(t, err)
			return database
		}
		writer, revoker := makeDB(), makeDB()
		mctx := merchant.WithID(ctx, owner.MerchantID)
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		var calls atomic.Int32
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(gateway.Close)
		t.Cleanup(unblock)
		done := make(chan error, 1)
		go func() {
			done <- writer.RunInMerchantConn(mctx, func(cctx context.Context) error {
				entered, err := providerqualification.WithWrite(cctx, writer, p.Target, func() error {
					req, err := http.NewRequestWithContext(cctx, http.MethodPost, gateway.URL, nil)
					if err != nil {
						return err
					}
					res, err := http.DefaultClient.Do(req)
					if err == nil {
						_ = res.Body.Close()
					}
					return err
				})
				if !entered && err == nil {
					t.Error("qualified write callback was not entered")
				}
				return err
			})
		}()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("write failed before provider: %v", err)
		}
		pid := make(chan int32, 1)
		revoked := make(chan error, 1)
		go func() {
			revoked <- revoker.RunInMerchantConn(mctx, func(cctx context.Context) error {
				var backend int32
				if err := revoker.Qx(cctx).QueryRow(cctx, "SELECT pg_backend_pid()").Scan(&backend); err != nil {
					return err
				}
				pid <- backend
				return providerqualification.Set(cctx, revoker, p.Target, nil)
			})
		}()
		var backend int32
		select {
		case backend = <-pid:
		case err := <-revoked:
			t.Fatalf("revocation finished before lock observation: %v", err)
		}
		require.Eventually(t, func() bool {
			var blocked bool
			err := h.Pool().QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock', false) FROM pg_stat_activity WHERE pid=$1`, backend).Scan(&blocked)
			return err == nil && blocked
		}, 5*time.Second, 5*time.Millisecond)
		unblock()
		require.NoError(t, <-done)
		require.NoError(t, <-revoked)
		require.NoError(t, writer.RunInMerchantConn(mctx, func(cctx context.Context) error {
			entered, err := providerqualification.WithWrite(cctx, writer, p.Target, func() error { calls.Add(1); return nil })
			require.False(t, entered)
			require.ErrorIs(t, err, providerqualification.ErrUnqualified)
			return nil
		}))
		require.Equal(t, int32(1), calls.Load(), "completed revocation prevents the next provider write")
	})
	for _, limited := range []bool{false, true} {
		name := "runner_heartbeat_commits_while_provider_is_blocked"
		if limited {
			name = "runner_heartbeat_cancels_when_single_pool_slot_is_busy"
		}
		t.Run(name, func(t *testing.T) {
			p := seed(t, "source_dark")
			database := rt.DB
			var resolver intents.NMIClientResolver = rt.CollectionResolver
			if limited {
				poolConfig, err := pgxpool.ParseConfig(h.DSN)
				require.NoError(t, err)
				poolConfig.MaxConns = 1
				pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
				require.NoError(t, err)
				t.Cleanup(pool.Close)
				database, err = db.NewWithPGXPool(pool, "openrails")
				require.NoError(t, err)
				clients := cutoverScopedClients{}
				for id, key := range map[uuid.UUID]string{p.Source: p.SourceKey, p.Target: p.TargetKey} {
					client, err := nmi.NewAccountClient(owner.MerchantID.UUID(), id, key, &config.NMIProviderSettings{SecurityKey: key}, true)
					require.NoError(t, err)
					client.V5BaseURL = g.Server.URL
					clients[id] = client
				}
				resolver = clients
			}
			fake := clockwork.NewFakeClockAt(time.Now().UTC())
			handler := &intents.NMIProviderCutover{DB: database, Resolver: resolver, Clock: fake}
			runner := rt.IntentRunner()
			store := &cutoverHeartbeatStore{Store: intents.NewStore(database), beats: make(chan cutoverRenewal, 2), attempts: make(chan struct{}, 2)}
			runner.Breaker, runner.Destructive = intents.NewVolumeBreaker(database), destructive.New(database)
			runner.Store, runner.Registry, runner.Clock, runner.Lease = store, intents.NewRegistry(handler), fake, 40*time.Second
			started, release := make(chan struct{}), make(chan struct{})
			var unblockOnce sync.Once
			unblock := func() { unblockOnce.Do(func() { close(release) }) }
			g.mu.Lock()
			g.AfterCreate = func() {
				close(started)
				select {
				case <-release:
				case <-t.Context().Done():
				}
			}
			g.mu.Unlock()
			t.Cleanup(func() { g.mu.Lock(); g.AfterCreate = nil; g.mu.Unlock() })
			done := make(chan error, 1)
			go func() {
				done <- database.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
					_, err := handler.Submit(cctx, runner, p.Sub, uuid.NewString(), p.Req, intents.OriginAdmin)
					return err
				})
			}()
			var finishOnce sync.Once
			var finishErr error
			finish := func() error { finishOnce.Do(func() { unblock(); finishErr = <-done }); return finishErr }
			t.Cleanup(func() { _ = finish() })
			select {
			case <-started:
			case err := <-done:
				finishOnce.Do(func() { finishErr = err })
				unblock()
				t.Fatalf("runner finished before provider gate: %v", err)
			}
			require.NoError(t, fake.BlockUntilContext(ctx, 1))
			var before, after time.Time
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT claimed_until FROM openrails.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&before))
			fake.Advance(10 * time.Second)
			<-store.attempts
			if limited {
				require.NoError(t, finish(), "stopping the Runner must cancel heartbeat acquisition while its caller still owns the only slot")
				beat := <-store.beats
				require.False(t, beat.renewed)
				require.ErrorIs(t, beat.err, context.Canceled)
			} else {
				beat := <-store.beats
				require.NoError(t, beat.err)
				require.True(t, beat.renewed)
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT claimed_until FROM openrails.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&after))
				require.True(t, after.After(before), "successful heartbeat must be visible outside the provider-lock transaction before HTTP returns")
				require.NoError(t, finish())
			}
		})

	}
}
