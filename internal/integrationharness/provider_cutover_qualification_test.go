//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
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
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM billing.destructive_action_switch`).Scan(&prior))
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.WithoutCancel(ctx), `UPDATE billing.destructive_action_switch SET enabled=$1`, prior)
		require.NoError(t, err)
	})
	_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=true`)
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

	t.Run("credential_rotation", func(t *testing.T) {
		// Only this loopback server receives the ordinary configuration API's
		// credential-query and simulated-auth probes.
		probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Authorization")
			if r.URL.Path == "/" {
				require.NoError(t, r.ParseForm())
				key = r.Form.Get("security_key")
			}
			g.mu.Lock()
			known := g.Accounts[key] != nil
			g.mu.Unlock()
			if !known {
				http.Error(w, "unknown fixture credential", 401)
				return
			}
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
			case "/payments/auth", "/payments/probe-txn/void":
				_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"SIMULATED","response_code":"100"}`))
			default:
				http.Error(w, "unexpected probe", 400)
			}
		}))
		t.Cleanup(probe.Close)
		rt.Merchants.SetCredentialProbeEndpointsForIntegration(probe.URL, "")
		rt.Merchants.SetNMIProbeV5EndpointForIntegration(probe.URL)
		mctx := merchant.WithID(ctx, owner.MerchantID)
		rotate := func(t *testing.T, p cutoverHTTPFixture, role, key string) {
			account := p.SourceKey
			enabled := false
			if role == "target" {
				account = p.TargetKey
				enabled = true
			}
			g.mu.Lock()
			g.Accounts[key] = g.Accounts[account]
			g.mu.Unlock()
			err := rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
				_, err := rt.Merchants.UpsertPaymentProviderConfig(cctx, owner.MerchantID, "nmi", merchants.UpsertPaymentProviderConfigRequest{AccountID: account, Enabled: &enabled, Credentials: map[string]string{"security_key": key}})
				return err
			})
			require.NoError(t, err)
		}
		resolve := func(id uuid.UUID, resolution intents.Resolution) error {
			return rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
				_, err := rt.IntentRunner().Resolve(cctx, id, resolution)
				return err
			})
		}
		qualify := func(t *testing.T, p cutoverHTTPFixture, role string) string {
			id := p.Source
			if role == "target" {
				id = p.Target
			}
			q := record(id)
			q.EvidenceRef = "rotation-proof-" + uuid.NewString()
			require.NoError(t, set(id, q))
			return q.EvidenceRef
		}
		terminal := map[uuid.UUID]intents.Resolution{}
		for _, mode := range []string{"complete_source", "complete_target", "abandon_source", "abandon_target"} {
			t.Run(mode, func(t *testing.T) {
				p := seed(t, "source_dark")
				key := uuid.NewString()
				operation, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				require.Equal(t, "unknown_needs_verify", operation.Status)
				var payload []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&payload))
				var oldSource, oldTarget *nmi.NMIClient
				require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					var ok bool
					var err error
					oldSource, ok, err = rt.ProviderCutovers.Resolver.ResolveNMIClient(cctx, owner.MerchantID.UUID(), &p.Source)
					if err != nil {
						return err
					}
					require.True(t, ok)
					oldTarget, ok, err = rt.ProviderCutovers.Resolver.ResolveNMIClient(cctx, owner.MerchantID.UUID(), &p.Target)
					require.True(t, ok)
					return err
				}))
				g.mu.Lock()
				g.Accounts[p.SourceKey].Mode = ""
				g.mu.Unlock()
				role := "source"
				if strings.HasSuffix(mode, "target") {
					role = "target"
				}
				rotate(t, p, role, "rotated-"+uuid.NewString())
				_, err = client.PreviewProviderCutover(ctx, openrails.SubscriptionID(p.Sub), p.Req)
				require.Error(t, err, "an old qualification cannot arm fresh credentials")
				stale := *rt.ProviderCutovers
				stale.Resolver = cutoverScopedClients{p.Source: oldSource, p.Target: oldTarget}
				err = rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error { _, err := stale.Preview(cctx, p.Sub, p.Req); return err })
				require.ErrorIs(t, err, providerqualification.ErrUnqualified, "an already armed client must see the shared version floor")
				proof := qualify(t, p, role)
				operation, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				require.Equal(t, "unknown_needs_verify", operation.Status, "account qualification alone cannot rewrite accepted credential identity")
				approval := intents.Resolution{Step: role, RequalifyAccount: proof, Actor: "fixture-operator", Reason: "provider account continuity independently verified"}
				require.NoError(t, resolve(operation.ID, approval))
				require.NoError(t, resolve(operation.ID, approval), "same resolution replays")
				var history []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT result_evidence->'account_requalifications' FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&history))
				require.NotEmpty(t, history)
				require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					store := intents.NewStore(rt.DB)
					for _, value := range []any{nil, []any{}, "forged"} {
						forged := map[string]any{"account_requalifications": value}
						require.Error(t, store.RecordProgress(cctx, operation.ID, forged))
						_, err := store.RecordProgressIfAbsent(cctx, operation.ID, "account_requalifications", value)
						require.Error(t, err)
						require.Error(t, store.MarkUnknown(cctx, operation.ID, time.Now(), "forged", forged))
						require.Error(t, store.MarkSucceeded(cctx, operation.ID, time.Now(), forged))
						require.Error(t, store.MarkFailedTerminal(cctx, operation.ID, "forged", forged))
						require.Error(t, store.PruneSucceeded(cctx, operation.ID, forged, false, false))
					}
					return nil
				}))
				if strings.HasPrefix(mode, "abandon") {
					require.NoError(t, resolve(operation.ID, intents.Resolution{Step: "target", Abandon: true, Actor: "fixture-operator", Reason: "cancel only the exact paused target"}))
				}
				operation, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				if strings.HasPrefix(mode, "complete") {
					assertCutoverCommitted(t, h, g, p, operation)
				} else {
					require.Equal(t, "failed_terminal", operation.Status)
					require.Equal(t, "abandoned", operation.Stage)
				}
				var after []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&after))
				require.JSONEq(t, string(payload), string(after))
				var retained []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT result_evidence->'account_requalifications' FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&retained))
				require.JSONEq(t, string(history), string(retained), "terminal outcome must retain the exact account-continuity record")
				require.NoError(t, resolve(operation.ID, approval), "the CLI resolution replays after terminal completion")
				terminal[operation.ID] = approval
				var evidence []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT result_evidence FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&evidence))
				require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					store := intents.NewStore(rt.DB)
					for _, keep := range [][2]bool{{false, false}, {true, false}, {false, true}} {
						require.NoError(t, store.PruneSucceeded(cctx, operation.ID, nil, keep[0], keep[1]))
					}
					require.NoError(t, store.PruneTerminalPayload(cctx, operation.ID))
					return nil
				}))
				var prunedPayload, prunedEvidence []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload,result_evidence FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&prunedPayload, &prunedEvidence))
				require.JSONEq(t, string(payload), string(prunedPayload))
				require.JSONEq(t, string(evidence), string(prunedEvidence), "generic pruning must preserve cutover replay and account custody")
			})
		}
		if len(terminal) == 4 {
			t.Run("terminal_rotation_history_archive_restore", func(t *testing.T) {
				// Only terminal operations are exportable. Deliver source host events
				// through the ordinary production queries before taking the archive.
				require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					events, err := rt.DB.Gen(cctx).ListHostEvents(cctx, gen.ListHostEventsParams{MerchantID: owner.MerchantID.UUID(), RowLimit: 100})
					if err != nil {
						return err
					}
					for _, event := range events {
						_, err = rt.DB.Gen(cctx).AcknowledgeHostEvent(cctx, gen.AcknowledgeHostEventParams{MerchantID: owner.MerchantID.UUID(), ID: event.ID, Now: time.Now().UTC()})
						if err != nil {
							return err
						}
					}
					return nil
				}))
				var artifact bytes.Buffer
				require.NoError(t, merchantarchive.Export(ctx, rt.DB, owner.MerchantID, &artifact))
				schema := "cutover_archive_" + uuid.NewString()[:8]
				dbtest.ApplyPostgresMigrations(t, h.SuperDSN, h.DSN, schema)
				target, err := db.NewDB(ctx, &config.DBConfig{URL: h.DSN, Schema: schema})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
				_, err = target.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, owner.MerchantID.UUID(), "restored-rotation")
				require.NoError(t, err)
				_, err = merchantarchive.Restore(ctx, target, owner.MerchantID, bytes.NewReader(artifact.Bytes()))
				require.NoError(t, err)
				runner := *rt.IntentRunner()
				runner.Store = intents.NewStore(target)
				for id, approval := range terminal {
					var before, after gen.OpenrailsRailIntent
					require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
						var err error
						before, err = intents.NewStore(rt.DB).Get(cctx, id)
						return err
					}))
					require.NoError(t, target.RunInMerchantConn(mctx, func(cctx context.Context) error {
						var err error
						after, err = runner.Resolve(cctx, id, approval)
						return err
					}), "the CLI resolution must replay from the restored terminal history without provider credentials")
					require.Equal(t, before.Status, after.Status)
					require.JSONEq(t, string(before.Payload), string(after.Payload))
					require.JSONEq(t, string(before.ResultEvidence), string(after.ResultEvidence))
				}
			})
		}
		t.Run("requalified_rotation_keeps_read_only_recovery_after_revocation", func(t *testing.T) {
			p := seed(t, "lost_activation")
			key := uuid.NewString()
			operation, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", operation.Status)
			rotate(t, p, "target", "rotated-"+uuid.NewString())
			proof := qualify(t, p, "target")
			require.NoError(t, resolve(operation.ID, intents.Resolution{Step: "target", RequalifyAccount: proof, Actor: "fixture-operator", Reason: "provider account continuity independently verified"}))
			require.NoError(t, set(p.Source, nil))
			require.NoError(t, set(p.Target, nil))
			operation, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			assertCutoverCommitted(t, h, g, p, operation)
		})
		t.Run("requalification_cannot_turn_another_account_404_into_evidence", func(t *testing.T) {
			p := seed(t, "source_dark")
			key := uuid.NewString()
			operation, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			wrongKey := "different-account-" + uuid.NewString()
			g.mu.Lock()
			g.Accounts[wrongKey] = &cutoverAccount{Source: true, Subs: map[string]nmi.V5Subscription{}}
			g.mu.Unlock()
			disabled := false
			require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
				_, err := rt.Merchants.UpsertPaymentProviderConfig(cctx, owner.MerchantID, "nmi", merchants.UpsertPaymentProviderConfigRequest{AccountID: p.SourceKey, Enabled: &disabled, Credentials: map[string]string{"security_key": wrongKey}})
				return err
			}))
			proof := qualify(t, p, "source")
			require.ErrorIs(t, resolve(operation.ID, intents.Resolution{Step: "source", RequalifyAccount: proof, Actor: "fixture-operator", Reason: "inadequate account continuity evidence"}), intents.ErrResolutionRejected)
			var records int
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT jsonb_array_length(COALESCE(result_evidence->'account_requalifications','[]')) FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&records))
			require.Zero(t, records)
			g.mu.Lock()
			deletes, activations := g.Accounts[wrongKey].Deletes, g.Accounts[p.TargetKey].Activations
			g.mu.Unlock()
			require.Zero(t, deletes)
			require.Zero(t, activations)
		})

		t.Run("ordinary_rotation_commits_its_floor_after_the_inflight_write", func(t *testing.T) {
			p := seed(t, "success")
			entered, release := make(chan int32, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			finished := make(chan error, 1)
			go func() {
				defer close(finished)
				finished <- rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					_, err := providerqualification.WithWrite(cctx, rt.DB, p.Target, providerqualification.Fingerprint(p.TargetKey), func() error {
						var backend int32
						if err := rt.DB.Qx(cctx).QueryRow(cctx, "SELECT pg_backend_pid()").Scan(&backend); err != nil {
							return err
						}
						entered <- backend
						<-release
						return nil
					})
					return err
				})
			}()
			t.Cleanup(func() { unblock(); <-finished })
			var writerBackend int32
			select {
			case writerBackend = <-entered:
			case err := <-finished:
				t.Fatalf("write did not reach its callback: %v", err)
			}
			replacement := "rotated-" + uuid.NewString()
			g.mu.Lock()
			g.Accounts[replacement] = g.Accounts[p.TargetKey]
			g.mu.Unlock()
			rotated := make(chan error, 1)
			go func() {
				defer close(rotated)
				rotated <- rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					_, err := rt.Merchants.UpsertPaymentProviderConfig(cctx, owner.MerchantID, "nmi", merchants.UpsertPaymentProviderConfigRequest{AccountID: p.TargetKey, Credentials: map[string]string{"security_key": replacement}})
					return err
				})
			}()
			t.Cleanup(func() { unblock(); <-rotated })
			require.Eventually(t, func() bool {
				var blocked bool
				err := h.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, writerBackend).Scan(&blocked)
				return err == nil && blocked
			}, 5*time.Second, 5*time.Millisecond)
			select {
			case err := <-rotated:
				t.Fatalf("rotation completed before provider callback returned: %v", err)
			default:
			}
			unblock()
			require.NoError(t, <-finished)
			require.NoError(t, <-rotated)
			calls := 0
			require.ErrorIs(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
				_, err := providerqualification.WithWrite(cctx, rt.DB, p.Target, providerqualification.Fingerprint(p.TargetKey), func() error { calls++; return nil })
				return err
			}), providerqualification.ErrUnqualified)
			require.Zero(t, calls, "a previously armed key cannot dispatch after completed rotation")
		})

		for _, abandon := range []bool{false, true} {
			name := "stale_executor_after_credential_ABA"
			if abandon {
				name = "stale_abandon_after_credential_ABA"
			}
			t.Run(name, func(t *testing.T) {
				p := seed(t, "source_dark")
				key := uuid.NewString()
				operation, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				g.mu.Lock()
				g.Accounts[p.SourceKey].Mode = ""
				g.mu.Unlock()
				if abandon {
					require.NoError(t, resolve(operation.ID, intents.Resolution{Step: "target", Abandon: true, Actor: "fixture-operator", Reason: "compensate paused target"}))
				}
				var snapshot gen.OpenrailsRailIntent
				require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
					var err error
					snapshot, err = intents.NewStore(rt.DB).Get(cctx, operation.ID)
					return err
				}))
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
				var caught atomic.Bool
				g.mu.Lock()
				g.AfterSubscriptionRead = func(source bool, _ nmi.V5Subscription) {
					if source != abandon && caught.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
				}
				g.mu.Unlock()
				done := make(chan intents.Outcome, 1)
				t.Cleanup(func() { releaseGate(); <-done; g.mu.Lock(); g.AfterSubscriptionRead = nil; g.mu.Unlock() })
				go func() {
					defer close(done)
					_ = rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error { done <- rt.ProviderCutovers.Execute(cctx, snapshot); return nil })
				}()
				select {
				case <-entered:
				case outcome := <-done:
					t.Fatalf("executor returned before the fixture read gate: %+v", outcome)
				}
				role, original := "source", p.SourceKey
				if abandon {
					role, original = "target", p.TargetKey
				}
				for _, replacement := range []string{"rotated-" + uuid.NewString(), original} {
					rotate(t, p, role, replacement)
					proof := qualify(t, p, role)
					require.NoError(t, resolve(operation.ID, intents.Resolution{Step: role, RequalifyAccount: proof, Actor: "fixture-operator", Reason: "verified same account after credential rotation"}))
				}
				releaseGate()
				<-done
				g.mu.Lock()
				g.AfterSubscriptionRead = nil
				g.mu.Unlock()
				g.mu.Lock()
				sourceDeletes, targetDeletes, targetActivations := g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Deletes, g.Accounts[p.TargetKey].Activations
				g.mu.Unlock()
				require.Zero(t, sourceDeletes)
				require.Zero(t, targetDeletes)
				require.Zero(t, targetActivations)
				var evidence struct {
					Records []json.RawMessage `json:"account_requalifications"`
				}
				var raw []byte
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT result_evidence FROM billing.rail_intents WHERE id=$1`, operation.ID).Scan(&raw))
				require.NoError(t, json.Unmarshal(raw, &evidence))
				require.Len(t, evidence.Records, 2, "stale progress cannot overwrite qualified history")
				operation, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				if abandon {
					require.Equal(t, "abandoned", operation.Stage)
				} else {
					assertCutoverCommitted(t, h, g, p, operation)
				}
			})
		}
	})

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
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&operations))
		require.Zero(t, operations)
	})

	t.Run("fresh_create_marker_refused_before_http_is_terminal", func(t *testing.T) {
		p := seed(t, "success")
		key := uuid.NewString()
		tx, err := h.Pool().Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
		_, err = tx.Exec(ctx, `SELECT id FROM billing.psps WHERE id=$1 FOR NO KEY UPDATE`, p.Target)
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
			err := h.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.rail_intents WHERE subscription_id=$1 AND result_evidence->>'create_submitted'='true')`, p.Sub).Scan(&marked)
			return err == nil && marked
		}, 5*time.Second, 5*time.Millisecond)
		_, err = tx.Exec(ctx, `UPDATE billing.psps SET evidence=evidence #- '{settings,nmi_cutover_qualification}' WHERE id=$1`, p.Target)
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
			database, err := db.NewWithPGXPool(pool, "billing")
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
				entered, err := providerqualification.WithWrite(cctx, writer, p.Target, providerqualification.Fingerprint(p.TargetKey), func() error {
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
				return providerqualification.Set(cctx, revoker, p.Target, nil, "", 0)
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
			entered, err := providerqualification.WithWrite(cctx, writer, p.Target, providerqualification.Fingerprint(p.TargetKey), func() error { calls.Add(1); return nil })
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
				database, err = db.NewWithPGXPool(pool, "billing")
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
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT claimed_until FROM billing.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&before))
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
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT claimed_until FROM billing.rail_intents WHERE subscription_id=$1`, p.Sub).Scan(&after))
				require.True(t, after.After(before), "successful heartbeat must be visible outside the provider-lock transaction before HTTP returns")
				require.NoError(t, finish())
			}
		})

	}
}
