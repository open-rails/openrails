//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Refuse only the terminal ledger write AFTER provider deletion. The local
// method must survive that rollback, then a fresh process verifies the same
// provider target and commits both local effects without another DELETE.
func TestNMIVaultDeleteAtomicRollbackAndProcessRecovery(t *testing.T) {
	type recovery struct {
		DSN, Gateway string
		Operation    uuid.UUID
	}
	if path := os.Getenv("OPENRAILS_NMI_DELETE_RECOVERY"); path != "" {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var input recovery
		require.NoError(t, json.Unmarshal(raw, &input))
		d := dbtest.OpenAppDB(t, input.DSN)
		client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "mobius", &config.NMIProviderSettings{SecurityKey: "test_security_key", WebhookSecret: "test_secret"}, true)
		require.NoError(t, err)
		client.LoopbackFixture = true
		client.V5BaseURL = input.Gateway
		client.QueryURL = input.Gateway
		client.DirectPostURL = input.Gateway
		runner := &Runner{Store: NewStore(d), Registry: NewRegistry(NewNMIPaymentMethodDeleteHandler(d, fakeVaultClientResolver{client: client})), Config: fullModeConfig(), Clock: clockwork.NewFakeClockAt(time.Now().UTC().Add(2 * time.Minute))}
		ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
		ctx, release, err := d.WithMerchantConn(ctx)
		require.NoError(t, err)
		defer release()
		_, err = runner.RunVerifyOnce(ctx)
		require.NoError(t, err)
		row, err := runner.Store.Get(ctx, input.Operation)
		require.NoError(t, err)
		require.Equal(t, StatusSucceeded, row.Status)
		method, payer, err := DeletedMethod(row)
		require.NoError(t, err)
		require.NotEqual(t, uuid.Nil, method)
		require.NotEqual(t, uuid.Nil, payer)
		return
	}
	fx := newVaultDeleteFixture(t)
	ddl := dbtest.SharedSuperuserPGXPool(t)
	name := "native_delete_rollback_" + uuid.NewString()[:8]
	_, err := ddl.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE billing.rail_intents ADD CONSTRAINT %s CHECK(NOT(idempotency_key='%s' AND status='succeeded'))`, name, NMIPaymentMethodDeleteIdempotencyKey(fx.pm.ID)))
	require.NoError(t, err)
	remove := func() {
		_, err := ddl.Exec(context.Background(), "ALTER TABLE billing.rail_intents DROP CONSTRAINT IF EXISTS "+name)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	out := fx.executeThrough(t)
	require.False(t, out.Done)
	require.False(t, fx.gateway.present.Load(), "provider deletion actually landed")
	require.True(t, fx.localRowExists(t), "terminal write failure must roll back local deletion")
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentStatus(t))
	operation, err := NewStore(fx.db).GetByIdempotencyKey(fx.ctx, NMIPaymentMethodDeleteIdempotencyKey(fx.pm.ID))
	require.NoError(t, err)
	remove()
	client := fx.runner.Registry.Lookup(TypeNMIPaymentMethodDelete).(*NMIPaymentMethodDeleteHandler).Rails.(fakeVaultClientResolver).client
	raw, err := json.Marshal(recovery{DSN: dbtest.SharedPostgresDSN(t), Gateway: client.V5BaseURL, Operation: operation.ID})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "recovery.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNMIVaultDeleteAtomicRollbackAndProcessRecovery$", "-test.v")
	command.Env = append(os.Environ(), "OPENRAILS_NMI_DELETE_RECOVERY="+path)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.False(t, fx.localRowExists(t))
	require.Equal(t, StatusSucceeded, fx.intentStatus(t))
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load(), "new process verifies without repeating deletion")
}

func TestNMIVaultDeleteFencesLateBillingAlias(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	fx.gateway.beforeDelete = make(chan struct{})
	fx.gateway.continueDelete = make(chan struct{})
	done := make(chan paymentmethods.PaymentMethodDeleteOutcome, 1)
	go func() { done <- fx.executeThrough(t) }()
	<-fx.gateway.beforeDelete
	alias := *fx.pm
	alias.ID = uuid.New()
	alias.RailMethodRef = "late-billing-entry"
	err := paymentmethods.NewPaymentMethodRepo(fx.db).Create(fx.ctx, &alias)
	close(fx.gateway.continueDelete)
	out := <-done
	require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodDeleteProcessing)
	require.True(t, out.Done)
	require.ErrorIs(t, paymentmethods.NewPaymentMethodRepo(fx.db).Create(fx.ctx, &alias), paymentmethods.ErrPaymentMethodDeleteUnsafe, "an erased whole vault must not be resurrected by a stale import")
}

func TestNMIVaultDeleteRefusesProviderOnlySibling(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	fx.gateway.extraBilling = true
	out := fx.executeThrough(t)
	require.False(t, out.Done)
	require.Zero(t, fx.gateway.vaultDeleteCalls.Load())
	require.Zero(t, fx.gateway.entryDeleteCalls.Load())
	require.True(t, fx.localRowExists(t))
	require.True(t, fx.gateway.present.Load())
}

func TestNMIVaultDeleteGenericCompletionCannotBypassRemoval(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	fx.gateway.deleteMode.Store("ambiguous500")
	out := fx.executeThrough(t)
	require.False(t, out.Done)
	row, err := NewStore(fx.db).GetByIdempotencyKey(fx.ctx, NMIPaymentMethodDeleteIdempotencyKey(fx.pm.ID))
	require.NoError(t, err)
	require.Error(t, NewStore(fx.db).MarkSucceeded(fx.ctx, row.ID, time.Now().UTC(), map[string]any{"deleted": true, "vault_id": fx.pm.RailCustomerRef}))
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentStatus(t))
	require.True(t, fx.localRowExists(t))
}

func TestNMIVaultAliasAdmissionRejectsNoncanonicalReferences(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	for _, part := range []string{"vault", "billing"} {
		alias := *fx.pm
		alias.ID = uuid.New()
		alias.RailMethodRef = "new-billing"
		if part == "vault" {
			alias.RailCustomerRef = " " + fx.pm.RailCustomerRef + " "
		} else {
			alias.RailMethodRef = " new-billing "
		}
		require.ErrorIs(t, paymentmethods.NewPaymentMethodRepo(fx.db).Create(fx.ctx, &alias), paymentmethods.ErrPaymentMethodDeleteUnsafe)
	}
	require.Zero(t, fx.gateway.vaultDeleteCalls.Load())
}

// A sole provider entry still cannot authorize erasing an unknown or different
// locally accepted card. This retains the refusal exposed by the retired
// legacy fixture, whose empty method reference contradicted provider entry B1.
func TestNMIVaultDeleteRefusesUnqualifiedSoleEntry(t *testing.T) {
	for _, ref := range []string{"", "different-billing-entry"} {
		name := ref
		if name == "" {
			name = "missing accepted billing entry"
		}
		t.Run(name, func(t *testing.T) {
			fx := newVaultDeleteFixture(t)
			fx.pm.RailMethodRef = ref
			_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, `UPDATE openrails.payment_methods SET rail_method_ref=$3 WHERE merchant_id=$1 AND id=$2`, dbtest.TestMerchantID.UUID(), fx.pm.ID, ref)
			require.NoError(t, err)
			out := fx.executeThrough(t)
			require.False(t, out.Done)
			require.Zero(t, fx.gateway.vaultDeleteCalls.Load())
			require.Zero(t, fx.gateway.entryDeleteCalls.Load())
			require.True(t, fx.localRowExists(t))
			require.Equal(t, StatusPending, fx.intentStatus(t))
		})
	}
}
