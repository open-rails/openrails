//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// Exercise the operator's real CLI over separate databases, then resume through
// the ordinary Client. This is core embedded/HTTP qualification, not a claim
// that the separately versioned SaaS host has adopted the current archive.
func TestOfflineBillingHandoff(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "openrails")
	build := exec.CommandContext(t.Context(), "go", "build", "-p", "2", "-o", binary, "./cmd/openrails")
	build.Dir = "../.."
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build normal CLI: %s", output)

	for _, rail := range []string{"nmi", "stripe"} {
		for _, sourceTopology := range []topology{embedded, remote} {
			targetTopology := remote
			if sourceTopology == remote {
				targetTopology = embedded
			}
			t.Run(rail+"/"+string(sourceTopology)+"_to_"+string(targetTopology), func(t *testing.T) {
				t.Parallel()
				source := newWorld(t)
				member := enroll(t, source, rail, sourceTopology)
				mid := source.client[embedded].MerchantID()
				end := member.periodEnd()
				initialPayments := source.payments(sourceTopology, member.c.id)
				require.Len(t, completed(initialPayments), 1)
				member.refreshBeforePeriodEnd()

				// Another merchant in the source database stays outside the export.
				sibling := source.rival()
				otherID := sibling.client.MerchantID()
				require.NotEqual(t, mid, otherID)
				sibling.server.Close()
				require.NoError(t, sibling.rt.Close(t.Context()))
				source.settle()
				events, err := source.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
				require.NoError(t, err)
				for _, event := range events.Items {
					_, err = source.client[embedded].AcknowledgeHostEvent(t.Context(), event.ID)
					require.NoError(t, err)
				}
				source.checkMoneyInvariants()
				source.stop()

				target := handoffTarget(t, source)
				cli := func(w *world, args ...string) (string, error) {
					t.Helper()
					configuration, err := json.Marshal(map[string]any{"schema": w.schema, "db": map[string]string{"url": w.dsn}})
					require.NoError(t, err)
					path := filepath.Join(t.TempDir(), "database.yaml")
					require.NoError(t, os.WriteFile(path, configuration, 0o600))
					cmd := exec.CommandContext(t.Context(), binary, append([]string{"--config", path, "billing"}, args...)...)
					cmd.Env = append(os.Environ(), "DB_URL="+w.dsn, "DB_SCHEMA="+w.schema)
					out, err := cmd.CombinedOutput()
					return string(out), err
				}
				run := func(w *world, args ...string) string {
					t.Helper()
					out, err := cli(w, args...)
					require.NoError(t, err, "%v: %s", args, out)
					return out
				}
				archive := filepath.Join(t.TempDir(), "merchant.jsonl")
				out, err := cli(source, "export", "--merchant", mid.String(), "--out", archive)
				require.Error(t, err)
				require.Contains(t, out, "--source-stopped")
				_, err = os.Stat(archive)
				require.ErrorIs(t, err, os.ErrNotExist)
				run(source, "export", "--source-stopped", "--merchant", mid.String(), "--out", archive)
				run(target, "prepare-target", "--unbound-merchants", "--merchant", mid.String(), "--slug", target.slug)
				run(target, "prepare-target", "--unbound-merchants", "--merchant", mid.String(), "--slug", target.slug)

				// Interrupted restore and missing target attestation cannot publish a
				// partial book. Target workers have never been constructed or started.
				out, err = cli(target, "import", "--merchant", mid.String(), "--in", archive)
				require.Error(t, err)
				require.Contains(t, out, "--target-stopped")
				complete, err := os.ReadFile(archive)
				require.NoError(t, err)
				partial := filepath.Join(t.TempDir(), "interrupted.jsonl")
				require.NoError(t, os.WriteFile(partial, complete[:len(complete)-20], 0o600))
				out, err = cli(target, "import", "--target-stopped", "--merchant", mid.String(), "--in", partial)
				require.Error(t, err, out)
				var count int
				require.NoError(t, target.pool.QueryRow(t.Context(), target.q("SELECT count(*) FROM billing.customers")).Scan(&count))
				require.Zero(t, count, "incomplete import rolled back all billing rows")
				require.Equal(t, 1, member.providerAttempts(), "preparation and interrupted import make no charge")

				// Treat the first successful receipt as lost; retry the same file.
				run(target, "import", "--target-stopped", "--merchant", mid.String(), "--in", archive)
				out = run(target, "import", "--target-stopped", "--merchant", mid.String(), "--in", archive)
				require.Contains(t, out, "already_imported=true")
				require.Equal(t, 1, member.providerAttempts(), "complete import and replay make no charge")
				require.NoError(t, target.pool.QueryRow(t.Context(), target.q("SELECT count(*) FROM billing.merchants WHERE id=$1"), otherID.UUID()).Scan(&count))
				require.Zero(t, count, "another source merchant was not imported")

				// Re-enter provider credentials through the normal constructor only
				// after restoration; preserved references resolve at the same gateway.
				target.start()
				// The destination observes the provider through its ordinary refresh
				// before taking over writes; imported progress is not a fresh receipt.
				target.refreshProviders()
				target.settleCollectionScans()
				require.Equal(t, 1, member.providerAttempts(), "destination catch-up before due sends no charge")
				require.Equal(t, mid, target.client[embedded].MerchantID())
				member.w, member.c.w, member.tp = target, target, targetTopology
				require.Equal(t, end, member.periodEnd())
				require.Equal(t, initialPayments[0].ID, target.payments(targetTopology, member.c.id)[0].ID)
				require.True(t, member.c.entitled(member.ent))
				target.restart()
				member.toPeriodEnd()
				target.runRenewals()
				target.runRenewals()
				require.Len(t, member.providerLedger(), 2, "one next-period charge at the destination")
				require.True(t, member.periodEnd().After(end))
				require.Empty(t, target.nmi.Schedules(), "engine billing did not create a provider schedule")
				target.stripe.mu.Lock()
				stripeSchedules := len(target.stripe.subs)
				target.stripe.mu.Unlock()
				require.Zero(t, stripeSchedules)

				_, err = target.client[targetTopology].CancelSubscription(t.Context(), member.sub, billing.CancelSubscriptionParams{Reason: "migrated customer cancels"})
				require.NoError(t, err)
				target.settle()
				require.NotNil(t, target.subscription(targetTopology, member.sub).CanceledAt)
				paid := completed(target.payments(targetTopology, member.c.id))
				require.Len(t, paid, 2)
				renewal := paid[0]
				if renewal.ID == initialPayments[0].ID {
					renewal = paid[1]
				}
				refundParams := billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "handoff-refund-" + renewal.ID.String()}
				refund, err := target.client[targetTopology].RefundPayment(t.Context(), renewal.ID, refundParams)
				require.NoError(t, err)
				target.settle()
				again, err := target.client[targetTopology].RefundPayment(t.Context(), renewal.ID, refundParams)
				require.NoError(t, err)
				require.Equal(t, refund.ID, again.ID)
				target.settle()
				var refunded int64
				for _, entry := range member.providerLedger() {
					refunded += entry.Refunded
				}
				require.EqualValues(t, member.amount, refunded)
				require.False(t, member.c.entitled(member.ent))
				target.advance(2 * monthHours * time.Hour)
				target.runRenewals()
				require.Len(t, member.providerLedger(), 2, "canceled destination subscription does not renew")
				require.NoError(t, source.pool.QueryRow(t.Context(), source.q("SELECT count(*) FROM billing.payments")).Scan(&count))
				require.Equal(t, 1, count, "stopped source remains at the final-export boundary")
			})
		}
	}
}

// The imported book gets an independent PostgreSQL database. Only provider
// fixtures and business time are shared; workers and local operation state are not.
func handoffTarget(t *testing.T, source *world) *world {
	t.Helper()
	name := "handoff_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	_, err := source.pool.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(source.dsn)
	require.NoError(t, err)
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	targetURL, err := url.Parse(source.dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, targetURL.Scheme)
	targetURL.Path = "/" + name
	target := &world{t: t, pool: pool, dsn: targetURL.String(), schema: source.schema, slug: source.slug,
		clock: source.clock, nmi: source.nmi, stripe: source.stripe,
		auth: &verifier{secret: []byte("handoff-" + uuid.NewString())}}
	t.Cleanup(func() {
		target.stop()
		pool.Close()
		_, err := source.pool.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
		require.NoError(t, err)
	})
	t.Cleanup(target.checkMoneyInvariants)
	require.NoError(t, openrails.Migrate(t.Context(), pool, openrails.Config{Schema: target.schema, RiverSchema: target.schema}))
	require.NoError(t, riverkit.ApplyMigrations(t.Context(), pool, target.schema))
	return target
}

// The group-bound preparation used by the hosted CLI checks real destination
// AuthKit authority. Neither the archive UUID nor a foreign owner can rebind it.
func TestBillingRestoreTargetUsesDestinationAuthority(t *testing.T) {
	t.Parallel()
	source := newWorld(t)
	mid := source.client[embedded].MerchantID()
	source.stop()
	target := handoffTarget(t, source)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), target.pool))
	client, err := openrails.New(t.Context(), openrails.Config{
		Schema: target.schema, RiverSchema: target.schema,
		TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
		DB: &openrails.DBConfig{URL: target.dsn},
		ControlPlane: &openrails.ControlPlaneConfig{LocalSignIn: true, Auth: openrails.AuthConfig{
			Issuer: "http://127.0.0.1/" + target.schema, AllowMemory: true, AllowMissingSenders: true,
			AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
		}},
	}, openrails.Deps{Postgres: target.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	auth := client.AuthKit()
	owner, outsider := authtest.NewUser(t, auth), authtest.NewUser(t, auth)
	ownerSubject := iam.UserSubject(owner.ID)
	group, err := auth.CreateGroup(t.Context(), iam.NewGroup{ID: uuid.NewString(), Persona: operator.MerchantType, Owner: &ownerSubject})
	require.NoError(t, err)
	req := operator.ProvisionMerchantForRestoreRequest{MerchantID: mid, Slug: target.slug, ExistingGroupID: group.ID, OwnerUserID: outsider.ID}
	_, err = operator.ProvisionMerchantForRestore(t.Context(), engine.Graph(client), req)
	require.ErrorIs(t, err, iam.ErrInsufficientAuthority)
	req.OwnerUserID = owner.ID
	prepared, err := operator.ProvisionMerchantForRestore(t.Context(), engine.Graph(client), req)
	require.NoError(t, err)
	require.Equal(t, mid, prepared.MerchantID)
	require.Equal(t, group.ID, prepared.GroupID)
	require.True(t, prepared.Created)
	again, err := operator.ProvisionMerchantForRestore(t.Context(), engine.Graph(client), req)
	require.NoError(t, err)
	require.False(t, again.Created)
	otherGroup, err := auth.CreateGroup(t.Context(), iam.NewGroup{ID: uuid.NewString(), Persona: operator.MerchantType, Owner: &ownerSubject})
	require.NoError(t, err)
	req.ExistingGroupID = otherGroup.ID
	_, err = operator.ProvisionMerchantForRestore(t.Context(), engine.Graph(client), req)
	require.ErrorIs(t, err, merchants.ErrMerchantRestoreConflict)
}
