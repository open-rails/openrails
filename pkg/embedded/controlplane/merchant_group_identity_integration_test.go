//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// TestMerchantGroupIdentity is the or#914 end-to-end proof: the merchant slug
// LIVES with its authkit permission group. One test drives, over the exported
// host surface against real Postgres + the real in-process authkit core:
//
//  1. authkit's generated POST /merchant (ak#263) creates group AND directory
//     row in one call, idempotently, with reserved-slug + admission gates;
//  2. the in-process ProvisionMerchant answers to the SAME declared policy for
//     user-claimed slugs while operator (ownerless) paths stay ungated;
//  3. an ak#264 slug rename forwards on every directory resolver (old slug,
//     new slug, webhook route) and lazily re-syncs the directory row;
//  4. DeleteGroup(ReleaseSlug) + a soft-deleted directory row frees the name
//     for a genuinely fresh merchant.
func TestMerchantGroupIdentity(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://or914.openrails.test")
	e := newHostApp(t, cfg)

	var refuse atomic.Bool
	sender := &captureEmailSender{}
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, e.App().Runtime.DB.Pool(), embcp.AttachOptions{
		HostedPosture: true,
		EmailSender:   sender,
		MerchantCreation: &embcp.MerchantCreationConfig{
			ReservedSlugs: []string{"or914-vip"},
			Admission: func(_ context.Context, slug, owner string) error {
				if refuse.Load() {
					return fmt.Errorf("card required before another merchant (or#914 test gate)")
				}
				return nil
			},
		},
	}))
	srv := mountAuthRoutes(t, e)
	cp := embcp.Get(e.App())
	require.NotSame(t, e.App().Runtime.DB.Pool(), cp.Pool().Raw(), "authority must have an independent pool when billing can pin its only connection")

	// A registered, verified user (the generated route requires a user subject).
	sfx := strings.ToLower(uuid.NewString()[:8])
	email := "or914-" + sfx + "@example.test"
	status, body := postJSON(t, srv.URL+"/register",
		`{"identifier":"`+email+`","username":"or914user`+sfx+`","password":"str0ng-horse-battery!"}`)
	require.Equal(t, http.StatusAccepted, status, "register: %v", body)
	status, body = postJSON(t, srv.URL+"/email/verify/confirm",
		`{"email":"`+email+`","code":"`+sender.code(email)+`"}`)
	require.Equal(t, http.StatusOK, status, "verify confirm: %v", body)
	token, _ := body["access_token"].(string)
	require.NotEmpty(t, token)
	user, err := cp.Core().GetUserByEmail(ctx, email)
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	postMerchant := func(slug string) (int, map[string]any) {
		t.Helper()
		req, rerr := http.NewRequest(http.MethodPost, srv.URL+"/merchant",
			strings.NewReader(`{"slug":"`+slug+`"}`))
		require.NoError(t, rerr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, rerr := http.DefaultClient.Do(req)
		require.NoError(t, rerr)
		defer resp.Body.Close()
		decoded := map[string]any{}
		_ = jsonDecode(resp, &decoded)
		return resp.StatusCode, decoded
	}

	directoryRow := func(slug string) (id, groupID string, found bool) {
		t.Helper()
		err := pool.QueryRow(ctx,
			`SELECT id::text, COALESCE(permission_group_id,'') FROM openrails.merchants WHERE slug = $1 AND deleted_at IS NULL`,
			slug).Scan(&id, &groupID)
		if err != nil {
			return "", "", false
		}
		return id, groupID, true
	}

	slug := "acme-" + sfx

	t.Run("generated route creates group AND directory row, idempotently", func(t *testing.T) {
		status, body := postMerchant(slug)
		require.Equal(t, http.StatusCreated, status, "create: %v", body)
		groupID, _ := body["group_id"].(string)
		require.NotEmpty(t, groupID, "ak#269: group_id on the create response")
		rowID, rowGroup, found := directoryRow(slug)
		require.True(t, found, "or#914: the wrap attaches the openrails.merchants row")
		require.Equal(t, groupID, rowGroup)
		require.NotEmpty(t, rowID)

		// Idempotent member re-run: 200, same group, still exactly one row.
		status, body = postMerchant(slug)
		require.Equal(t, http.StatusOK, status, "re-create: %v", body)
		require.Equal(t, false, body["created"])
		require.Equal(t, groupID, body["group_id"])
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.merchants WHERE slug = $1`, slug).Scan(&n))
		require.Equal(t, 1, n)
	})

	t.Run("reserved slugs and the admission gate refuse over the route", func(t *testing.T) {
		status, body := postMerchant("or914-vip")
		require.GreaterOrEqual(t, status, 400, "reserved slug must refuse: %v", body)
		require.Less(t, status, 500)
		_, _, found := directoryRow("or914-vip")
		require.False(t, found, "no directory row for a refused claim")

		refuse.Store(true)
		defer refuse.Store(false)
		status, body = postMerchant("blocked-" + sfx)
		require.GreaterOrEqual(t, status, 400, "admission refusal must surface: %v", body)
		require.Less(t, status, 500)
		_, gerr := cp.Core().ResolveGroupIDForSlug(ctx, "merchant", "blocked-"+sfx)
		require.Error(t, gerr, "no group is created behind a refused admission")
	})

	t.Run("in-process user claims answer to the same policy; operator paths stay ungated", func(t *testing.T) {
		_, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "or914-vip", OwnerUserID: user.ID,
		})
		require.ErrorIs(t, err, embcp.ErrSlugReserved)
		_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "platform", OwnerUserID: user.ID,
		})
		require.ErrorIs(t, err, embcp.ErrSlugReserved, "the advisory hosted default list applies too")

		refuse.Store(true)
		_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: "gated-" + sfx, OwnerUserID: user.ID,
		})
		require.ErrorIs(t, err, embcp.ErrCreationRefused)
		refuse.Store(false)

		// Ownerless provisioning is an OPERATOR act (openrails-saas's platform
		// merchant bootstrap claims the reserved "platform" name this way).
		res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "platform"})
		require.NoError(t, err, "reserved slugs stay claimable through operator paths")
		require.True(t, res.Created)
	})

	t.Run("ak#264 rename forwards on every directory resolver and re-syncs the row", func(t *testing.T) {
		oldSlug := "alpha-" + sfx
		newSlug := "beta-" + sfx
		res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: oldSlug, OwnerUserID: user.ID,
		})
		require.NoError(t, err)
		require.NoError(t, cp.Core().RenamePermissionGroupSlugAs(ctx, user.ID, "merchant", oldSlug, newSlug))

		// NEW slug: the directory row is stale; group resolution finds it and
		// lazily re-syncs the row.
		mid, canonical, err := cp.ResolveMerchantForGroup(ctx, newSlug)
		require.NoError(t, err, "new slug resolves while the directory row is stale")
		require.Equal(t, res.MerchantID.String(), mid.String())
		require.Equal(t, newSlug, canonical)
		var rowSlug string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT slug FROM openrails.merchants WHERE id = $1::uuid`, res.MerchantID.String()).Scan(&rowSlug))
		require.Equal(t, newSlug, rowSlug, "directory row lazily re-synced to the group's current slug")

		// An active former-name alias resolves directly to the group; canonical name wins.
		mid, canonical, err = cp.ResolveMerchantForGroup(ctx, oldSlug)
		require.NoError(t, err, "renamed-away slug forwards (ak#264)")
		require.Equal(t, res.MerchantID.String(), mid.String())
		require.Equal(t, newSlug, canonical)

		// Webhook-route resolution (published URLs carry the old slug).
		dir, err := merchants.NewDirectoryService(db.WrapPool(pool, "openrails"))
		require.NoError(t, err)
		dir.WithGroupSlugResolver(cp.MerchantGroupSlugResolver()).WithGroupIDResolver(cp.MerchantGroupIDResolver())
		route, err := dir.ResolveBySlug(ctx, oldSlug)
		require.NoError(t, err, "webhook URLs keep resolving across a rename")
		require.Equal(t, res.MerchantID.String(), route.MerchantID.String())
		require.Equal(t, newSlug, route.MerchantSlug)
		canonicalByID, err := dir.CanonicalSlug(ctx, res.MerchantID)
		require.NoError(t, err)
		require.Equal(t, newSlug, canonicalByID)

		// Provisioning through an active alias is idempotent for its existing
		// group. It must not create a fresh billing identity.
		aliased, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: oldSlug, OwnerUserID: user.ID,
		})
		require.NoError(t, err)
		require.False(t, aliased.Created)
		require.Equal(t, res.MerchantID, aliased.MerchantID)
		require.Equal(t, res.GroupID, aliased.GroupID)
	})

	t.Run("reclaimed name cannot select a former owner's live billing row", func(t *testing.T) {
		name := "reclaim-" + sfx
		original, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: name, OwnerUserID: user.ID})
		require.NoError(t, err)
		stranger, err := cp.Core().CreateUser(ctx, "other-"+sfx+"@example.test", "other"+sfx)
		require.NoError(t, err)
		// Reproduce the former crash window: AuthKit released a name while the
		// old billing directory row is still live. No alias/local projection may
		// make the new group's authority select that old row.
		require.NoError(t, cp.Core().DeleteGroupInstanceByID(ctx, original.GroupID, authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))
		replacement, err := cp.Core().CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: name, OwnerSubjectID: stranger.ID})
		require.NoError(t, err)
		_, _, err = cp.ResolveAuthorizedMerchant(ctx, name, stranger.ID, "merchant:catalog:read")
		require.ErrorIs(t, err, policy.ErrMerchantUnresolved)
		dir, err := merchants.NewDirectoryService(db.WrapPool(pool, "openrails"))
		require.NoError(t, err)
		_, _, err = dir.Provision(ctx, merchants.ProvisionRequest{Slug: name, PermissionGroupID: replacement})
		require.ErrorIs(t, err, merchants.ErrMerchantBindingConflict)
		unchanged, err := dir.Get(ctx, original.MerchantID)
		require.NoError(t, err)
		require.Equal(t, original.GroupID, unchanged.PermissionGroupID)
		_, err = pool.Exec(ctx, `UPDATE openrails.merchants SET deleted_at=now(),status='deleted' WHERE id=$1`, original.MerchantID.UUID())
		require.NoError(t, err)
		fresh, made, err := dir.Provision(ctx, merchants.ProvisionRequest{Slug: name, PermissionGroupID: replacement})
		require.NoError(t, err)
		require.True(t, made)
		require.NotEqual(t, original.MerchantID, fresh.ID)
		got, _, err := cp.ResolveAuthorizedMerchant(ctx, name, stranger.ID, "merchant:catalog:read")
		require.NoError(t, err)
		require.Equal(t, fresh.ID, got)
		_, _, err = cp.ResolveAuthorizedMerchant(ctx, name, user.ID, "merchant:catalog:read")
		require.ErrorIs(t, err, policy.ErrPermissionRequired)
		// A delayed release retry is anchored to the first group's UUID.
		require.NoError(t, cp.Core().DeleteGroupInstanceByID(ctx, original.GroupID, authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))
		alive, err := cp.Core().GroupInstanceByID(ctx, replacement)
		require.NoError(t, err)
		require.Equal(t, name, alive.InstanceSlug)
	})

	t.Run("retirement resumes by UUID after external deletion", func(t *testing.T) {
		name := "retire-" + sfx
		original, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: name, OwnerUserID: user.ID})
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE openrails.merchants SET created_at=now()-interval '3 days' WHERE id=$1`, original.MerchantID.UUID())
		require.NoError(t, err)
		dir, err := merchants.NewDirectoryService(db.WrapPool(pool, "openrails"))
		require.NoError(t, err)
		cfg := merchants.DormancySweepConfig{TTL: 24 * time.Hour, WarningLead: time.Hour, Armed: true}
		failed := true
		release := func(ctx context.Context, groupID string) error {
			require.Equal(t, original.GroupID, groupID, "release must use captured UUID")
			require.NoError(t, cp.Core().DeleteGroupInstanceByID(ctx, groupID, authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))
			if failed {
				return errors.New("crash after group commit")
			}
			return nil
		}
		_, err = dir.SweepDormant(ctx, cfg, release)
		require.NoError(t, err)
		require.NoError(t, db.WrapPool(pool, "openrails").MerchantTx(ctx, original.MerchantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE openrails.merchant_dormancy_notices SET first_warned_at=now()-interval '2 hours' WHERE merchant_id=$1`, original.MerchantID.UUID())
			require.EqualValues(t, 1, tag.RowsAffected())
			return err
		}))
		outcome, err := dir.SweepDormant(ctx, cfg, release)
		require.ErrorContains(t, err, "crash after group commit")
		require.Zero(t, outcome.Deleted)
		_, err = pool.Exec(ctx, `UPDATE openrails.merchants SET deleted_at=NULL,status='active' WHERE id=$1`, original.MerchantID.UUID())
		require.ErrorContains(t, err, "cannot be restored")
		fresh, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: name, OwnerUserID: user.ID})
		require.NoError(t, err)
		require.NotEqual(t, original.GroupID, fresh.GroupID)
		failed = false
		restarted, err := merchants.NewDirectoryService(db.WrapPool(pool, "openrails"))
		require.NoError(t, err)
		outcome, err = restarted.SweepDormant(ctx, cfg, release)
		require.NoError(t, err)
		require.Equal(t, 1, outcome.Deleted)
		_, err = cp.Core().GroupInstanceByID(ctx, fresh.GroupID)
		require.NoError(t, err, "retry must not delete reclaimed name")
	})

	t.Run("released names are claimable again; tombstoned directory rows do not pin them", func(t *testing.T) {
		gone := "gone-" + sfx
		res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: gone, OwnerUserID: user.ID,
		})
		require.NoError(t, err)

		// Host-side delete: soft-delete the directory row, release the name in
		// authkit (the never-used-merchant dormancy shape; plain deletes
		// tombstone by default).
		_, err = pool.Exec(ctx, `UPDATE openrails.merchants
			SET status='deleted', deleted_at=now(), updated_at=now() WHERE id=$1::uuid`,
			res.MerchantID.String())
		require.NoError(t, err)
		require.NoError(t, cp.Core().DeletePermissionGroup(ctx, "merchant", gone,
			authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))

		res2, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
			Slug: gone, OwnerUserID: user.ID,
		})
		require.NoError(t, err, "a released name is claimable again (or#914 live-rows-only uniqueness)")
		require.True(t, res2.Created)
		require.NotEqual(t, res.MerchantID.String(), res2.MerchantID.String(),
			"the re-claim is a NEW merchant, not a resurrection")

		// The dead row still resolves nowhere.
		mid, _, err := cp.ResolveMerchantForGroup(ctx, gone)
		require.NoError(t, err)
		require.Equal(t, res2.MerchantID.String(), mid.String())
	})
}

func jsonDecode(resp *http.Response, into *map[string]any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("no body")
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
