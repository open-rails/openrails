//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

func hostProvision(t *testing.T, w *standaloneWorld, slug string) billing.ProvisionMerchantResult {
	t.Helper()
	var m billing.ProvisionMerchantResult
	w.command(t, map[string]any{"Op": "host-provision", "Body": billing.ProvisionMerchantParams{Slug: slug}}, &m)
	return m
}

func TestMerchantHost(t *testing.T) {
	t.Run("OR-J32-S01/provision-find-list-rename-retains-identity", func(t *testing.T) {
		w := newStandaloneWorld(t)
		m := hostProvision(t, w, "host-shop")
		require.True(t, m.Created)
		require.Equal(t, m.MerchantID.String(), m.GroupID)
		replay := hostProvision(t, w, "host-shop")
		require.False(t, replay.Created)
		require.Equal(t, m.MerchantID, replay.MerchantID)
		var entry billing.Merchant
		w.command(t, map[string]any{"Op": "host-get", "Merchant": m.MerchantID}, &entry)
		require.Equal(t, "host-shop", entry.Slug)
		require.Equal(t, billing.MerchantActive, entry.Status)
		var members []billing.UserMerchant
		var assigned bool
		w.command(t, map[string]any{"Op": "host-membership", "Merchant": m.MerchantID, "ID": w.AliceID}, &assigned)
		w.command(t, map[string]any{"Op": "host-memberships", "ID": w.AliceID}, &members)
		require.Len(t, members, 1)
		require.Equal(t, m.MerchantID, members[0].ID)
		w.command(t, map[string]any{"Op": "host-memberships", "ID": w.BobID}, &members)
		require.Empty(t, members)
		var renamed billing.MerchantName
		w.command(t, map[string]any{"Op": "host-rename", "Merchant": m.MerchantID, "Body": billing.RenameMerchantParams{Name: "host-renamed"}}, &renamed)
		require.Equal(t, m.MerchantID, renamed.ID)
		for _, name := range []string{"host-shop", "host-renamed"} {
			var found billing.MerchantName
			w.command(t, map[string]any{"Op": "host-find", "ID": name}, &found)
			require.Equal(t, m.MerchantID, found.ID)
			require.Equal(t, "host-renamed", found.Name)
		}
		var page billing.ListPage[billing.Merchant]
		w.command(t, map[string]any{"Op": "host-list", "Body": billing.MerchantListParams{Query: "host-renamed"}}, &page)
		require.Len(t, page.Items, 1)
		require.Equal(t, m.MerchantID, page.Items[0].ID)
	})
	t.Run("OR-J32-S02/concurrent-name-claim-and-cooldown", func(t *testing.T) {
		w := newStandaloneWorld(t)
		first := hostProvision(t, w, "first-shop")
		second := hostProvision(t, w, "second-shop")
		var results []struct {
			Result *billing.MerchantName
			Error  string
		}
		w.command(t, map[string]any{"Op": "host-concurrent-renames", "Body": []any{map[string]any{"Merchant": first.MerchantID, "Name": "single-owner"}, map[string]any{"Merchant": second.MerchantID, "Name": "single-owner"}}}, &results)
		require.Len(t, results, 2)
		successes := 0
		var winner billing.MerchantID
		for _, result := range results {
			if result.Error == "" {
				successes++
				require.NotNil(t, result.Result)
				winner = result.Result.ID
			} else {
				require.Contains(t, result.Error, "taken")
			}
		}
		require.Equal(t, 1, successes)
		var found billing.MerchantName
		w.command(t, map[string]any{"Op": "host-find", "ID": "single-owner"}, &found)
		require.Equal(t, winner, found.ID)
		err := w.hostResult(t, map[string]any{"Op": "host-rename", "Merchant": winner, "Account": w.AliceID, "Body": billing.RenameMerchantParams{Name: "too-soon"}}, nil)
		require.Contains(t, err, "next rename")
		w.command(t, map[string]any{"Op": "host-find", "ID": "single-owner"}, &found)
		require.Equal(t, winner, found.ID)
		require.NotEmpty(t, w.hostResult(t, map[string]any{"Op": "host-find", "ID": "too-soon"}, nil))
	})
	t.Run("OR-J32-S03/used-retirement-refused-and-deletion-closes-authority", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.Merchants["alpha"]
		beta := w.Merchants["beta"]
		price := hostProduct(t, w.alpha, "retained-catalog")
		var retirement billing.MerchantRetirement
		w.command(t, map[string]any{"Op": "host-retire", "Merchant": alpha.ID, "ID": alpha.ID.String()}, &retirement)
		require.False(t, retirement.Retired)
		require.Equal(t, billing.MerchantRetirementRefusedActive, retirement.Refusal)
		before, err := w.alpha.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.EqualValues(t, 10_000_000, before.UnitAmount)
		w.command(t, map[string]any{"Op": "host-retire", "Merchant": beta.ID, "ID": alpha.ID.String()}, &retirement)
		require.Equal(t, billing.MerchantRetirementRefusedGroupMismatch, retirement.Refusal)
		var deleted billing.Merchant
		w.command(t, map[string]any{"Op": "host-delete", "Merchant": alpha.ID}, &deleted)
		require.Equal(t, billing.MerchantDeleted, deleted.Status)
		for _, token := range []string{alpha.Key, alpha.CustomerToken} {
			status, raw := w.request(t, "GET", "/v1/me/config", token, nil)
			require.Contains(t, []int{401, 403, 404}, status, string(raw))
		}
		status, raw := w.request(t, "GET", "/v1/me/config", beta.CustomerToken, nil)
		require.Equal(t, 200, status, string(raw))
		var page billing.ListPage[billing.Merchant]
		w.command(t, map[string]any{"Op": "host-list", "Body": billing.MerchantListParams{}}, &page)
		for _, item := range page.Items {
			require.NotEqual(t, alpha.ID, item.ID)
		}
		var restored billing.Merchant
		w.command(t, map[string]any{"Op": "host-restore", "Merchant": alpha.ID}, &restored)
		require.Equal(t, billing.MerchantActive, restored.Status)
		after, err := w.alpha.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.Equal(t, before.ID, after.ID)
		require.Equal(t, before.UnitAmount, after.UnitAmount)
	})
	t.Run("OR-J32-S04/retirement-retry-preserves-reclaimed-name-owner", func(t *testing.T) {
		w := newStandaloneWorld(t)
		old := hostProvision(t, w, "retirable")
		var key iam.APIKeyCreated
		w.command(t, map[string]any{"Op": "host-key", "Merchant": old.MerchantID}, &key)
		var retirement billing.MerchantRetirement
		w.command(t, map[string]any{"Op": "host-retire", "Merchant": old.MerchantID, "ID": old.GroupID}, &retirement)
		require.True(t, retirement.Retired)
		require.NotEmpty(t, w.hostResult(t, map[string]any{"Op": "host-restore", "Merchant": old.MerchantID}, nil))
		fresh := hostProvision(t, w, "retirable")
		require.True(t, fresh.Created)
		require.NotEqual(t, old.MerchantID, fresh.MerchantID)
		var freshKey iam.APIKeyCreated
		w.command(t, map[string]any{"Op": "host-key", "Merchant": fresh.MerchantID}, &freshKey)
		for range 2 {
			var completed int
			w.command(t, map[string]any{"Op": "host-complete-retirements"}, &completed)
			require.Zero(t, completed)
		}
		status, raw := w.request(t, "GET", "/v1/admin/customers", key.Secret, nil)
		require.Contains(t, []int{401, 403}, status, string(raw))
		status, raw = w.request(t, "GET", "/v1/admin/customers", freshKey.Secret, nil)
		require.Equal(t, 200, status, string(raw))
		var found billing.MerchantName
		w.command(t, map[string]any{"Op": "host-find", "ID": "retirable"}, &found)
		require.Equal(t, fresh.MerchantID, found.ID)
		w.command(t, map[string]any{"Op": "host-retire", "Merchant": old.MerchantID, "ID": old.GroupID}, &retirement)
		require.False(t, retirement.Retired)
		require.Equal(t, billing.MerchantRetirementRefusedNotLive, retirement.Refusal)
	})
	t.Run("OR-J32-S05/cli-purge-queues-its-vault-cleanup", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		psp, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		merchant := a.alpha.MerchantID().UUID()
		feeReads := func() int {
			var n int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "stripe_fee_reads"}.Sanitize()+" WHERE merchant_id=$1", merchant).Scan(&n))
			return n
		}
		_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "stripe_fee_reads"}.Sanitize()+" (merchant_id, psp_id, ref, kind, due_at) VALUES ($1, $2, 'ch_purged', 'payment', 'infinity')",
			merchant, psp.ID.UUID())
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "operation_log"}.Sanitize()+` (merchant_id, actor, source, object_type, object_id, action, changes, rail, psp_id)
			VALUES ($1, 'system', 'intent', 'provider_intent', 'op_purged', 'attempting', '{"version": 1}', 'stripe', $2)`, merchant, psp.ID.UUID())
		require.NoError(t, err)
		documents := func() []any {
			listed, err := a.deps.Vault.Logical().ListWithContext(t.Context(), a.cfg.Vault.KVMount+"/metadata/openrails/merchants/"+merchant.String()+"/psps")
			require.NoError(t, err)
			if listed == nil {
				return nil
			}
			keys, _ := listed.Data["keys"].([]any)
			return keys
		}
		require.NotEmpty(t, documents(), "the PSP's credentials live in Vault")
		enableLifecycleMaintenance(t, a)
		vault := map[string]any{"vault": map[string]any{"address": a.deps.Vault.Address(), "token": a.deps.Vault.Token(), "kv_mount": a.cfg.Vault.KVMount}}
		purge := func(out any, args ...string) error {
			stdout, err := hostCLICommand(t, a.world, vault, append([]string{"merchant", "purge", "--merchant", "alpha", "--by", "critical operator"}, args...)...).Output()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("%w: %s", err, exit.Stderr)
			}
			if err != nil {
				return err
			}
			return json.Unmarshal(stdout, out)
		}

		var inventory billing.MerchantPurgeInventory
		require.NoError(t, purge(&inventory))
		require.Equal(t, "alpha", inventory.Name)
		require.Contains(t, inventory.Documents, "psps/stripe")
		require.Equal(t, "purge merchant alpha permanently, no backup exists", inventory.ConfirmPhrase)
		confirm := []string{"--inventory", inventory.ID.String(), "--confirm", inventory.ConfirmPhrase}
		require.ErrorContains(t, purge(nil, append(confirm, "--expect-rows", strconv.Itoa(inventory.TotalRows+1))...), "refusing to purge")
		var purged billing.MerchantPurge
		require.NoError(t, purge(&purged, append(confirm, "--expect-rows", strconv.Itoa(inventory.TotalRows))...))
		require.True(t, purged.CleanupQueued)
		require.Equal(t, inventory.TotalRows, purged.Rows)
		require.Equal(t, 1, inventory.RowCounts["stripe_fee_reads"])
		require.Zero(t, feeReads(), "a fee read waiting on its PSP goes with the merchant")
		require.Equal(t, 1, inventory.RowCounts["operation_log"])
		var logged int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "operation_log"}.Sanitize()+" WHERE merchant_id=$1", merchant).Scan(&logged))
		require.Zero(t, logged, "the purge deletes the operations history its guard refuses anyone else")
		var deleted bool
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT deleted_at IS NOT NULL FROM "+pgx.Identifier{a.schema, "merchants"}.Sanitize()+" WHERE id=$1", merchant).Scan(&deleted))
		require.True(t, deleted, "the purge tombstones the merchant")
		require.NotEmpty(t, documents(), "the cleanup waits for a worker")

		fleet := hostFleet(t, a.world)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		run := func() string {
			var status string
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{a.schema, "maintenance_runs"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", merchant, purged.RunID.UUID()).Scan(&status))
			return status
		}
		require.Eventually(t, func() bool { return run() == "completed" && len(documents()) == 0 },
			30*time.Second, 100*time.Millisecond, "the job queued with the purge deletes the merchant's Vault configuration")
	})
	t.Run("OR-J32-S07/a-purge-the-event-store-refused-to-delete-from-is-retried-by-its-job", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		usageMeter(t, a.alpha, 100)
		customer := billing.CustomerID(uuid.New())
		fund(t, a.alpha, customer, "purged-funds", 1000)
		settleUsage(t, a.alpha, newLease(), numbered(1, usageInput(customer, "purged", 2))...)
		enableLifecycleMaintenance(t, a)
		overlay := map[string]any{
			"vault":      map[string]any{"address": a.deps.Vault.Address(), "token": a.deps.Vault.Token(), "kv_mount": a.cfg.Vault.KVMount},
			"clickhouse": map[string]string{"url": "clickhouse://" + os.Getenv("OPENRAILS_CRITICAL_CLICKHOUSE_ADDR") + "/" + a.schema},
		}
		purge := func(out any, args ...string) error {
			stdout, err := hostCLICommand(t, a.world, overlay, append([]string{"merchant", "purge", "--merchant", "alpha", "--by", "critical operator"}, args...)...).Output()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("%w: %s", err, exit.Stderr)
			}
			if err != nil {
				return err
			}
			return json.Unmarshal(stdout, out)
		}
		rows := func(tables ...string) uint64 {
			t.Helper()
			var total uint64
			for _, table := range tables {
				var n uint64
				require.NoError(t, a.deps.ClickHouse.QueryRow(t.Context(), "SELECT count() FROM `"+a.schema+"`."+table+" WHERE merchant_id = toUUID(?)",
					a.alpha.MerchantID().String()).Scan(&n))
				total += n
			}
			return total
		}
		var inventory billing.MerchantPurgeInventory
		require.NoError(t, purge(&inventory))
		// The store refuses the deletion from one of its tables.
		store := "`" + a.schema + "`"
		require.NoError(t, a.deps.ClickHouse.Exec(t.Context(), "RENAME TABLE "+store+".limit_usage TO "+store+".limit_usage_away"))
		var purged billing.MerchantPurge
		require.NoError(t, purge(&purged, "--inventory", inventory.ID.String(), "--confirm", inventory.ConfirmPhrase, "--expect-rows", strconv.Itoa(inventory.TotalRows)),
			"the purge commits; the store's deletion is its job's")
		require.NotZero(t, rows("usage_history"), "the store kept the merchant's usage")
		require.NoError(t, a.deps.ClickHouse.Exec(t.Context(), "RENAME TABLE "+store+".limit_usage_away TO "+store+".limit_usage"))

		fleet := hostFleet(t, a.world)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		require.Eventually(t, func() bool {
			return rows("usage_events", "limit_usage", "usage_history") == 0
		}, 60*time.Second, 200*time.Millisecond, "the job queued with the purge deletes the merchant's usage from the store")
	})
	t.Run("OR-J32-S06/purge-removes-the-directory-copy-and-its-tokens", func(t *testing.T) {
		a, _ := newPSPWorld(t, func(w *world) { w.deps.UserInfo = nil })
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "owned directory"})
		require.NoError(t, err)
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": uuid.NewString(), "userName": "purged-contact", "displayName": "Purged Contact"}
		status, raw := a.request(t, "POST", "/v1/app/scim/v2/Users", token.Token, user)
		require.Equal(t, 201, status, string(raw))
		rows := func(table string) int {
			t.Helper()
			var n int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, table}.Sanitize()+" WHERE merchant_id = $1", a.alpha.MerchantID().UUID()).Scan(&n))
			return n
		}
		require.Equal(t, 1, rows("customer_contacts"))
		require.Equal(t, 1, rows("provisioning_tokens"))
		enableLifecycleMaintenance(t, a)
		vault := map[string]any{"vault": map[string]any{"address": a.deps.Vault.Address(), "token": a.deps.Vault.Token(), "kv_mount": a.cfg.Vault.KVMount}}
		purge := func(out any, args ...string) error {
			stdout, err := hostCLICommand(t, a.world, vault, append([]string{"merchant", "purge", "--merchant", "alpha", "--by", "critical operator"}, args...)...).Output()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("%w: %s", err, exit.Stderr)
			}
			if err != nil {
				return err
			}
			return json.Unmarshal(stdout, out)
		}
		var inventory billing.MerchantPurgeInventory
		require.NoError(t, purge(&inventory))
		require.Equal(t, 1, inventory.RowCounts["customer_contacts"], "the inventory counts the directory's copy")
		require.Equal(t, 1, inventory.RowCounts["provisioning_tokens"])
		var purged billing.MerchantPurge
		require.NoError(t, purge(&purged, "--inventory", inventory.ID.String(), "--confirm", inventory.ConfirmPhrase, "--expect-rows", strconv.Itoa(inventory.TotalRows)))
		require.Zero(t, rows("customer_contacts"), "the merchant's copy of its directory goes with it")
		require.Zero(t, rows("provisioning_tokens"), "and the tokens its directory pushed with")
	})
	t.Run("OR-J32-S08/cli-create-sets-the-display-name-vault-holds", func(t *testing.T) {
		w := newWorld(t)
		t.Cleanup(func() {
			_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema + "_auth"}.Sanitize()+" CASCADE")
		})
		addr, token := os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"), os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN")
		require.NotEmpty(t, addr, "critical configuration cases require real Vault")
		t.Setenv(vaulttest.AddrEnv, addr)
		t.Setenv(vaulttest.TokenEnv, token)
		vault := vaulttest.New(t)
		keys := staffKeys(t)
		overlay := map[string]any{"vault": map[string]any{"address": vault.Addr, "token": vault.Token, "kv_mount": vault.Mount}}
		// run is the operator CLI with merchant configuration in Vault; it
		// answers stderr and decodes stdout into out.
		run := func(out any, args ...string) string {
			t.Helper()
			cmd := staffCLI(t, w, keys, overlay, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Run(), "%v: %s", args, stderr.String())
			require.NoError(t, json.Unmarshal(stdout.Bytes(), out), stdout.String())
			return stderr.String()
		}

		// task dev's seed.
		var created billing.Merchant
		run(&created, "merchants", "create", "sandbox", "--display-name", "Sandbox")
		require.Equal(t, "sandbox", created.Slug)
		require.NotNil(t, created.DisplayName)
		require.Equal(t, "Sandbox", *created.DisplayName)
		var doc struct {
			DisplayName string `json:"display_name"`
		}
		require.NotZero(t, vault.Get(t, "openrails/merchants/"+created.ID.String()+"/merchant", &doc), "the merchant document is Vault's")
		require.Equal(t, "Sandbox", doc.DisplayName)

		// The seed again, as every task dev start runs it.
		var again billing.Merchant
		require.Contains(t, run(&again, "merchants", "create", "sandbox", "--display-name", "Sandbox"), "sandbox exists; unchanged")
		require.Equal(t, created.ID, again.ID)
		var read billing.Merchant
		run(&read, "merchants", "get", "sandbox")
		require.NotNil(t, read.DisplayName)
		require.Equal(t, "Sandbox", *read.DisplayName)
	})
}
