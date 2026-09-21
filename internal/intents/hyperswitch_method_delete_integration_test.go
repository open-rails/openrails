//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type deletionSecrets struct{}

func (deletionSecrets) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{Value: "delete-key", Version: 1}, nil
}
func (s deletionSecrets) GetAtLeastVersion(ctx context.Context, m merchant.ID, name string, _ int) (merchants.Secret, error) {
	return s.Get(ctx, m, name)
}

type custodyDeleteFixture struct {
	db        *db.DB
	ctx       context.Context
	pm        *models.PaymentMethod
	custodian uuid.UUID
	runner    *Runner
	handler   *HyperSwitchMethodDeleteHandler
	clock     *clockwork.FakeClock
	calls     atomic.Int64
	erased    atomic.Bool
	mode      atomic.Value
}

func newCustodyDeleteFixture(t *testing.T) *custodyDeleteFixture {
	t.Helper()
	f := &custodyDeleteFixture{db: dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()), ctx: merchant.WithID(t.Context(), dbtest.TestMerchantID), custodian: uuid.New(), clock: clockwork.NewFakeClockAt(time.Now().UTC())}
	f.mode.Store("success")
	pool := f.db.Pool()
	dbtest.EnsureTestMerchant(f.ctx, t, pool)
	psp := dbtest.EnsureTestPSP(f.ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")
	customer := dbtest.EnsureCustomerIDPgx(f.ctx, t, pool, uuid.NewString())
	_, err := pool.Exec(f.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$3,'{"profile_id":"profile_A","public_api_key":"public_A"}','{"api_key":1}')`, f.custodian, dbtest.TestMerchantID.UUID(), "delete_"+f.custodian.String())
	require.NoError(t, err)
	f.pm = &models.PaymentMethod{ID: uuid.New(), CustomerID: customer, Rail: models.RailNMI, PspID: psp, Custodian: models.CustodianHyperSwitch, CustodianID: &f.custodian, RailCustomerRef: "customer_A", RailMethodRef: "method_A", ChargeVia: "pan_proxy", RebillDriver: models.RebillDriverOpenRails}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.db).Create(f.ctx, f.pm))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "api-key=delete-key", r.Header.Get("Authorization"))
		require.Equal(t, "profile_A", r.Header.Get("x-profile-id"))
		w.Header().Set("Content-Type", "application/json")
		mode := f.mode.Load().(string)
		if r.Method == http.MethodGet && r.URL.Path == "/v2/proxy" {
			if mode == "capability absent" {
				fmt.Fprint(w, `{"contract":"openrails-nmi-form-v2"}`)
				return
			}
			fmt.Fprint(w, `{"contract":"openrails-nmi-form-v2","native_vault_delete_contract":"openrails-native-vault-delete-v1"}`)
			return
		}
		require.Equal(t, http.MethodDelete, r.Method)
		require.Equal(t, "/v2/payment-methods/method_A", r.URL.Path)
		f.calls.Add(1)
		if mode == "missing" {
			w.WriteHeader(404)
			return
		}
		f.erased.Store(true)
		if mode == "lost" {
			w.WriteHeader(503)
			return
		}
		if mode == "wrong receipt" {
			fmt.Fprint(w, `{"id":"other_method"}`)
			return
		}
		fmt.Fprint(w, `{"id":"method_A"}`)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: server.URL}}
	rails := &paymentmethods.RailPaymentMethodService{DB: f.db, Config: cfg, MerchantSecrets: deletionSecrets{}}
	f.handler = NewHyperSwitchMethodDeleteHandler(f.db, rails, f.clock)
	f.runner = &Runner{Store: NewStore(f.db), Registry: NewRegistry(f.handler), Config: cfg, Clock: f.clock}
	return f
}
func (f *custodyDeleteFixture) run(t *testing.T, pm *models.PaymentMethod) paymentmethods.PaymentMethodDeleteOutcome {
	t.Helper()
	out, err := (&PaymentMethodDeleteThrough{Runner: f.runner}).ExecutePaymentMethodDelete(f.ctx, pm)
	require.NoError(t, err)
	return out
}
func (f *custodyDeleteFixture) operation(t *testing.T) gen.OpenrailsRailIntent {
	t.Helper()
	row, err := NewStore(f.db).GetByIdempotencyKey(f.ctx, TypeHyperSwitchMethodDelete+":"+f.pm.ID.String())
	require.NoError(t, err)
	return row
}
func (f *custodyDeleteFixture) resume(t *testing.T) {
	t.Helper()
	f.clock.Advance(time.Hour)
	_, err := f.runner.RunVerifyOnce(f.ctx)
	require.NoError(t, err)
	f.clock.Advance(time.Hour)
	_, err = f.runner.RunExecuteOnce(f.ctx)
	require.NoError(t, err)
}

func TestHyperSwitchDeletionRetainsExactTargetThroughUncertainty(t *testing.T) {
	for _, mode := range []string{"success", "lost", "missing", "wrong receipt", "capability absent", "readonly", "local rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyDeleteFixture(t)
			f.mode.Store(mode)
			if mode == "readonly" {
				f.handler.Rails.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
			}
			var remove func()
			if mode == "local rollback" {
				ddl := dbtest.SharedSuperuserPGXPool(t)
				name := "delete_rollback_" + uuid.NewString()[:8]
				_, err := ddl.Exec(f.ctx, fmt.Sprintf(`ALTER TABLE billing.rail_intents ADD CONSTRAINT %s CHECK(NOT (idempotency_key='%s' AND status='succeeded'))`, name, TypeHyperSwitchMethodDelete+":"+f.pm.ID.String()))
				require.NoError(t, err)
				remove = func() {
					_, err := ddl.Exec(context.Background(), "ALTER TABLE billing.rail_intents DROP CONSTRAINT IF EXISTS "+name)
					require.NoError(t, err)
				}
				t.Cleanup(remove)
			}
			out := f.run(t, f.pm)
			if mode != "success" {
				require.False(t, out.Done)
				method, err := f.db.Gen(f.ctx).GetPaymentMethodByID(f.ctx, gen.GetPaymentMethodByIDParams{MerchantID: dbtest.TestMerchantID.UUID(), ID: f.pm.ID})
				require.NoError(t, err)
				row := f.operation(t)
				require.Equal(t, "delete:"+row.ID.String(), method.ParkReason)
				require.NotEqual(t, StatusSucceeded, row.Status)
				if mode == "readonly" || mode == "capability absent" {
					require.Zero(t, f.calls.Load())
				}
				if mode == "local rollback" {
					require.True(t, f.erased.Load())
					remove()
				}
				f.mode.Store("success")
				f.handler.Rails.Config.ProviderWriteMode = config.ProviderWriteModeFull
				f.resume(t)
			}
			require.True(t, f.erased.Load())
			require.Equal(t, StatusSucceeded, f.operation(t).Status)
			_, err := paymentmethods.NewPaymentMethodRepo(f.db).GetByID(f.ctx, f.pm.ID)
			require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodNotFound)
			before := f.calls.Load()
			require.True(t, f.run(t, f.pm).Done)
			require.Equal(t, before, f.calls.Load(), "terminal replay never re-deletes")
		})
	}
}

func TestHyperSwitchDeletionDetachesOnlyAnUnusedOwnedAlias(t *testing.T) {
	f := newCustodyDeleteFixture(t)
	other := *f.pm
	other.ID = uuid.New()
	other.PspID = dbtest.EnsureTestPSP(f.ctx, t, f.db.Pool(), dbtest.TestMerchantID.UUID(), "other_delete_psp")
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.db).Create(f.ctx, &other))
	require.True(t, f.run(t, f.pm).Done)
	require.Zero(t, f.calls.Load())
	require.True(t, f.run(t, &other).Done)
	require.EqualValues(t, 1, f.calls.Load())
}

func TestHyperSwitchDeletionRefusesForeignAliasesAndPinnedOperations(t *testing.T) {
	for _, mode := range []string{"foreign alias", "pending", "failed_retryable", "in_flight", "unknown_needs_verify"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyDeleteFixture(t)
			if mode == "foreign alias" {
				other := *f.pm
				other.ID = uuid.New()
				other.CustomerID = dbtest.EnsureCustomerIDPgx(f.ctx, t, f.db.Pool(), uuid.NewString())
				other.PspID = dbtest.EnsureTestPSP(f.ctx, t, f.db.Pool(), dbtest.TestMerchantID.UUID(), "foreign_delete_psp")
				require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.db).Create(f.ctx, &other))
			} else {
				_, err := f.db.Pool().Exec(f.ctx, `INSERT INTO billing.rail_intents(id,merchant_id,rail,psp_id,intent_type,payload,idempotency_key,status,next_attempt_at,origin) VALUES($1,$2,'nmi',$3,'invoice_collection',jsonb_build_object('payment_method_id',$4::text),$5,$6,now(),'user')`, uuid.New(), dbtest.TestMerchantID.UUID(), f.pm.PspID, f.pm.ID.String(), uuid.NewString(), mode)
				require.NoError(t, err)
			}
			_, err := (&PaymentMethodDeleteThrough{Runner: f.runner}).ExecutePaymentMethodDelete(f.ctx, f.pm)
			require.Error(t, err)
			require.Zero(t, f.calls.Load())
			method, err := f.db.Gen(f.ctx).GetPaymentMethodByID(f.ctx, gen.GetPaymentMethodByIDParams{MerchantID: dbtest.TestMerchantID.UUID(), ID: f.pm.ID})
			require.NoError(t, err)
			require.Empty(t, method.ParkReason)
		})
	}
}

func TestHyperSwitchPendingDetachSerializesAliasDecisions(t *testing.T) {
	f := newCustodyDeleteFixture(t)
	other := *f.pm
	other.ID = uuid.New()
	other.PspID = dbtest.EnsureTestPSP(f.ctx, t, f.db.Pool(), dbtest.TestMerchantID.UUID(), "pending_detach_psp")
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.db).Create(f.ctx, &other))
	f.handler.Rails.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
	require.False(t, f.run(t, f.pm).Done)
	first := f.operation(t)
	p, err := DecodeHyperSwitchMethodDelete(first)
	require.NoError(t, err)
	require.True(t, p.DetachOnly)
	_, err = (&PaymentMethodDeleteThrough{Runner: f.runner}).ExecutePaymentMethodDelete(f.ctx, &other)
	require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodDeleteProcessing, "another accepted handle decision must resolve first")
	f.handler.Rails.Config.ProviderWriteMode = config.ProviderWriteModeFull
	f.clock.Advance(time.Hour)
	row, err := f.runner.ExecuteByID(f.ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)
	require.Zero(t, f.calls.Load(), "first alias was detached locally")
	require.True(t, f.run(t, &other).Done)
	require.EqualValues(t, 1, f.calls.Load(), "exactly the final reference erases the vendor card")
}

func TestStoredCardDeleteMaintenanceExceptionRequiresVerifiedPayer(t *testing.T) {
	for _, mode := range []string{"unattributed", "foreign payer", "forged user origin"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyDeleteFixture(t)
			f.runner.Destructive = destructive.New(f.db)
			if mode == "unattributed" {
				require.False(t, f.run(t, f.pm).Done)
				require.Equal(t, string(OriginAdmin), f.operation(t).Origin)
			} else if mode == "foreign payer" {
				other := dbtest.EnsureCustomerIDPgx(f.ctx, t, f.db.Pool(), uuid.NewString())
				f.ctx = billingauth.SetUserContext(f.ctx, billingauth.UserContext{UserID: other.String()})
				forged := *f.pm
				forged.CustomerID = other
				_, err := (&PaymentMethodDeleteThrough{Runner: f.runner}).ExecutePaymentMethodDelete(f.ctx, &forged)
				require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodDeleteUnsafe)
			} else {
				_, err := NewStore(f.db).Enqueue(f.ctx, EnqueueParams{MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", PspID: f.pm.PspID, IntentType: TypeNMIPaymentMethodDelete, IdempotencyKey: NMIPaymentMethodDeleteIdempotencyKey(f.pm.ID), Origin: OriginUser, Actor: f.pm.CustomerID.String(), Payload: NMIPaymentMethodDeletePayload{UserID: f.pm.CustomerID.String(), PaymentMethodID: f.pm.ID, RailCustomerRef: f.pm.RailCustomerRef, RailMethodRef: f.pm.RailMethodRef}, NextAttemptAt: f.clock.Now()})
				require.ErrorContains(t, err, "authenticated payer")
			}
			require.Zero(t, f.calls.Load())
		})
	}
}
