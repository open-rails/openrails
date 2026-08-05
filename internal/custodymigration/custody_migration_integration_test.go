//go:build integration

package custodymigration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/custodymigration"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#297 Phase C, end to end against real Postgres. The vendor legs (Basis
// Theory's proxy, the NMI gateway behind it) are fake HTTP servers — there is
// no BT tenant and no live gateway call in a test — but every DB-side
// assertion is real: instrument rows, the custody-migration record,
// subscriptions, in-flight intents, RLS-scoped writes.
//
// The story these tests tell: a processor de-platforms the merchant, the
// merchant obtains a vault export, the custodian ingests it, and the SAME
// card file keeps billing — through a DIFFERENT NMI gateway account.

// ---------------------------------------------------------------- fixture

type custodyFixture struct {
	ctx        context.Context
	db         *db.DB
	merchants  *merchants.Service
	custodian  merchants.CustodianScope
	oldPSP     gen.OpenrailsPsp // the de-platforming gateway; holds its own vault
	newPSP     gen.OpenrailsPsp // the survivor gateway; charges through the custodian
	productID  uuid.UUID
	priceID    uuid.UUID
	env        string
}

const (
	oldGatewaySecurityKey = "sk_deplatformed_gateway"
	newGatewaySecurityKey = "sk_survivor_gateway"
)

func newCustodyFixture(t *testing.T) *custodyFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := context.Background()
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())
	mctx := merchant.WithID(ctx, dbtest.TestMerchantID)

	store, err := merchants.NewDBSecretStore(dbi.DataPool())
	require.NoError(t, err)
	env := config.ExpectedProviderEnvironment(true)
	svc, err := merchants.NewService(dbi.DataPool(), store, env)
	require.NoError(t, err)

	sfx := uuid.NewString()[:8]
	fx := &custodyFixture{ctx: mctx, db: dbi, merchants: svc, env: env}

	// The custodian: one merchant-owned Basis Theory account.
	custodian, err := svc.UpsertCustodian(ctx, dbtest.TestMerchantID, config.CustodianEntry{
		Key:       "bt-" + sfx,
		Kind:      models.CustodianBasisTheory,
		AccountID: "tnt_or297_" + sfx,
		Settings:  map[string]any{custodians.SettingPublicAPIKey: "key_pub_" + sfx},
	}, env)
	require.NoError(t, err)
	fx.custodian = custodian
	putSecret(t, svc, custodianSecret(t, custodian), "key_private_"+sfx)
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.custodians WHERE id = $1`, custodian.ID)
	})

	// Two NMI gateway accounts. The old one holds its own customer vault; the
	// new one charges through the custodian — the deplatforming survival move.
	fx.oldPSP = seedPSP(t, dbi, svc, env, "gw-old-"+sfx, nil, oldGatewaySecurityKey)
	fx.newPSP = seedPSP(t, dbi, svc, env, "gw-new-"+sfx, &custodian.ID, newGatewaySecurityKey)

	// A catalog row for the subscriptions to hang off; nothing here is under test.
	fx.productID, fx.priceID = uuid.New(), uuid.New()
	_, err = dbi.Pool().Exec(mctx,
		`INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1, $2, $3, 'or297 custody')`,
		fx.productID, dbtest.TestMerchantID.UUID(), "or297-"+sfx)
	require.NoError(t, err)
	_, err = dbi.Pool().Exec(mctx,
		`INSERT INTO openrails.prices (id, merchant_id, product_id, key, amount, currency) VALUES ($1, $2, $3, $4, 1999, 'USD')`,
		fx.priceID, dbtest.TestMerchantID.UUID(), fx.productID, "or297-"+sfx)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = dbi.Pool().Exec(bg, `DELETE FROM openrails.prices WHERE id = $1`, fx.priceID)
		_, _ = dbi.Pool().Exec(bg, `DELETE FROM openrails.products WHERE id = $1`, fx.productID)
	})
	return fx
}

func custodianSecret(t *testing.T, c merchants.CustodianScope) string {
	t.Helper()
	name, err := merchants.CustodianSecretName(c.Kind, c.Environment, c.AccountID, custodians.SecretAPIKey)
	require.NoError(t, err)
	return name
}

func putSecret(t *testing.T, svc *merchants.Service, name, value string) {
	t.Helper()
	_, err := svc.Secrets().Put(context.Background(), dbtest.TestMerchantID, name, value)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Secrets().Delete(context.Background(), dbtest.TestMerchantID, name) })
}

func seedPSP(t *testing.T, dbi *db.DB, svc *merchants.Service, env, accountID string, custodianID *uuid.UUID, securityKey string) gen.OpenrailsPsp {
	t.Helper()
	name, err := merchants.PSPSecretName(string(models.RailNMI), env, accountID, "security_key")
	require.NoError(t, err)
	putSecret(t, svc, name, securityKey)

	var row gen.OpenrailsPsp
	mctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		r, uerr := dbi.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:  dbtest.TestMerchantID.UUID(),
			Rail:        string(models.RailNMI),
			Environment: &env,
			AccountID:   accountID,
			CustodianID: custodianID,
		})
		row = r
		return uerr
	}))
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.psps WHERE id = $1`, row.ID)
	})
	return row
}

// seedPSPVaultedCard mints the instrument a de-platforming strands: an NMI
// customer-vault entry, held BY the processor, with a live stored-credential
// anchor and a subscription riding it.
func (fx *custodyFixture) seedPSPVaultedCard(t *testing.T, vaultID string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	methodID := uuid.New()
	// One customer per seeded card: the lifecycle uniqueness constraint allows
	// a customer only one live subscription per product.
	customerID := dbtest.EnsureCustomerIDPgx(fx.ctx, t, fx.db.Pool(), uuid.NewString())
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, fx.db.RunInMerchantConn(fx.ctx, func(ctx context.Context) error {
		_, err := fx.db.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
			ID:              methodID,
			MerchantID:      dbtest.TestMerchantID.UUID(),
			CustomerID:      customerID,
			Rail:            string(models.RailNMI),
			RailCustomerRef: vaultID,
			PspID:           &fx.oldPSP.ID,
			Custodian:       models.CustodianPSP,
			LastFour:        strPtr("4242"),
			CardType:        strPtr("visa"),
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		return err
	}))
	// The anchor: an existing credential-on-file sequence that must survive.
	_, err := fx.db.Pool().Exec(fx.ctx,
		`UPDATE openrails.payment_methods SET stored_credential_unscheduled_ref = $2, stored_credential_recurring_ref = $3 WHERE id = $1`,
		methodID, "anchor-unsched-"+vaultID, "anchor-recur-"+vaultID)
	require.NoError(t, err)

	subID := uuid.New()
	_, err = fx.db.Pool().Exec(fx.ctx,
		`INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, rail, rail_subscription_id, status, payment_method_id, psp_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'nmi', $6, 'active', $7, $8, now(), now())`,
		subID, dbtest.TestMerchantID.UUID(), customerID, fx.productID, fx.priceID, "railsub-"+vaultID, methodID, fx.oldPSP.ID)
	require.NoError(t, err)

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.rail_intents WHERE subscription_id = $1`, subID)
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.subscriptions WHERE id = $1`, subID)
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.custody_migrations WHERE payment_method_id = $1`, methodID)
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.payment_methods WHERE id = $1`, methodID)
	})
	return methodID, subID
}

func strPtr(s string) *string { return &s }

func (fx *custodyFixture) method(t *testing.T, id uuid.UUID) gen.OpenrailsPaymentMethod {
	t.Helper()
	var row gen.OpenrailsPaymentMethod
	require.NoError(t, fx.db.RunInMerchantConn(fx.ctx, func(ctx context.Context) error {
		r, err := fx.db.Gen(ctx).GetPaymentMethodByID(ctx, id)
		row = r
		return err
	}))
	return row
}

// export builds a manifest naming the survivor gateway as the post-flip PSP.
func (fx *custodyFixture) export(tokens ...custodymigration.ImportedToken) custodymigration.VaultExport {
	n := len(tokens)
	return custodymigration.VaultExport{
		ExportedAt:     time.Now().UTC().Add(-time.Hour),
		SourceRail:     string(models.RailNMI),
		Custodian:      fx.custodian.Key,
		PSP:            custodymigration.PSPRef{Rail: string(models.RailNMI), Environment: fx.env, AccountID: fx.newPSP.AccountID},
		ExpectedTokens: &n,
		Tokens:         tokens,
	}
}

func (fx *custodyFixture) opts(exp custodymigration.VaultExport, apply bool) custodymigration.Options {
	return custodymigration.Options{
		PGXPool:    fx.db.Pool(),
		MerchantID: dbtest.TestMerchantID,
		Export:     exp,
		Apply:      apply,
	}
}

// ---------------------------------------------------------------- tests

// TestCustodyMigration_PlanThenApply is the mechanism's spine: a dry run
// reports what would happen and writes NOTHING; the apply flips custody on the
// SAME payment_method_id, keeps the dead PSP vault handle, leaves the
// subscription and the stored-credential anchors alone, and records the move.
func TestCustodyMigration_PlanThenApply(t *testing.T) {
	fx := newCustodyFixture(t)
	vaultID := "vault-" + uuid.NewString()[:8]
	methodID, subID := fx.seedPSPVaultedCard(t, vaultID)
	token := "tok_" + uuid.NewString()[:12]

	before := fx.method(t, methodID)
	var subUpdatedBefore time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT updated_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subUpdatedBefore))

	exp := fx.export(custodymigration.ImportedToken{
		SourceRailCustomerRef: vaultID,
		Token:                 token,
		Fingerprint:           "fp_" + uuid.NewString()[:10],
		LastFour:              "1111",
		CardType:              "visa",
		ExpiryDate:            "12/31",
	})

	// PLAN: the counts arrive before a single row moves.
	plan, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, false))
	require.NoError(t, err)
	require.False(t, plan.Applied)
	require.Equal(t, 1, plan.Counts[custodymigration.OutcomeRemapped])
	require.Equal(t, vaultID, plan.Rows[0].FromRailCustomerRef)
	require.Equal(t, methodID, *plan.Rows[0].PaymentMethodID)
	require.Equal(t, before, fx.method(t, methodID), "a plan writes nothing")
	requireMigrationCount(t, fx, methodID, 0)

	// APPLY.
	res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.Equal(t, 1, res.Counts[custodymigration.OutcomeRemapped])

	after := fx.method(t, methodID)
	require.Equal(t, methodID, after.ID, "the instrument identity NEVER moves — that is the whole seam")
	require.Equal(t, models.CustodianBasisTheory, after.Custodian)
	require.Equal(t, token, after.RailMethodRef)
	require.Equal(t, string(models.RailNMI), after.Rail, "custody moves; the gateway KIND does not (or#879)")
	require.Equal(t, vaultID, after.RailCustomerRef, "the dead PSP vault handle is retained — it is the only link to charges made before the flip")
	require.Equal(t, exp.Tokens[0].Fingerprint, after.Fingerprint)
	require.Equal(t, nmiproxy.ViaPANProxy, after.ChargeVia)
	require.NotNil(t, after.PspID)
	require.Equal(t, fx.newPSP.ID, *after.PspID, "the survivor gateway now settles this card")
	require.Equal(t, before.StoredCredentialUnscheduledRef, after.StoredCredentialUnscheduledRef,
		"credential-on-file anchors are gateway-scoped, not custody-scoped")
	require.Equal(t, before.StoredCredentialRecurringRef, after.StoredCredentialRecurringRef)
	require.Equal(t, "1111", *after.LastFour)

	// The subscription did not move, and was not even written.
	var subMethod uuid.UUID
	var subUpdatedAfter time.Time
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT payment_method_id, updated_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subMethod, &subUpdatedAfter))
	require.Equal(t, methodID, subMethod)
	require.Equal(t, subUpdatedBefore, subUpdatedAfter, "a custody flip is not a subscription event")

	// The record: reversible IN RECORD (every old handle is here), never in custody.
	rec := migrationRecord(t, fx, methodID)
	require.Equal(t, string(custodymigration.OutcomeRemapped), rec.Outcome)
	require.Equal(t, models.CustodianPSP, rec.FromCustodian)
	require.Equal(t, vaultID, rec.FromRailCustomerRef)
	require.Equal(t, models.CustodianBasisTheory, rec.ToCustodian)
	require.Equal(t, token, rec.ToRailMethodRef)
	require.Equal(t, fx.custodian.ID, rec.ToCustodianID)
	require.NotNil(t, rec.FromPspID)
	require.Equal(t, fx.oldPSP.ID, *rec.FromPspID)
	require.NotNil(t, rec.ToPspID)
	require.Equal(t, fx.newPSP.ID, *rec.ToPspID)
	require.Equal(t, res.BatchID, rec.BatchID)

	// Idempotent: the same manifest again is a no-op with one record, not two.
	again, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.Equal(t, 1, again.Counts[custodymigration.OutcomeAlreadyMigrated])
	require.Zero(t, again.Counts[custodymigration.OutcomeRemapped])
	requireMigrationCount(t, fx, methodID, 1)
}

// TestCustodyMigration_RefusesMidDunningAttempt: no charge may straddle the
// flip. An instrument whose subscription has an intent in flight (or sent and
// unverified) is REFUSED — and the refusal is transient, so the same manifest
// succeeds once the attempt resolves.
func TestCustodyMigration_RefusesMidDunningAttempt(t *testing.T) {
	for _, status := range []string{"in_flight", "unknown_needs_verify"} {
		t.Run(status, func(t *testing.T) {
			fx := newCustodyFixture(t)
			vaultID := "vault-" + uuid.NewString()[:8]
			methodID, subID := fx.seedPSPVaultedCard(t, vaultID)
			intentID := seedChargeIntent(t, fx, subID, status)

			exp := fx.export(custodymigration.ImportedToken{
				SourceRailCustomerRef: vaultID, Token: "tok_" + uuid.NewString()[:12],
			})

			plan, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, false))
			require.NoError(t, err)
			require.Equal(t, custodymigration.OutcomeBlocked, plan.Rows[0].Outcome)
			require.Equal(t, custodymigration.ReasonChargeInFlight, plan.Rows[0].Reason)

			res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
			require.NoError(t, err)
			require.Equal(t, 1, res.Counts[custodymigration.OutcomeBlocked])
			require.Equal(t, models.CustodianPSP, fx.method(t, methodID).Custodian,
				"a refused instrument is untouched")
			requireMigrationCount(t, fx, methodID, 0)

			// The attempt resolves; the operator re-runs; the card moves.
			_, err = fx.db.Pool().Exec(fx.ctx,
				`UPDATE openrails.rail_intents SET status = 'succeeded', executed_at = now() WHERE id = $1`, intentID)
			require.NoError(t, err)

			res, err = custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
			require.NoError(t, err)
			require.Equal(t, 1, res.Counts[custodymigration.OutcomeRemapped])
			require.Equal(t, models.CustodianBasisTheory, fx.method(t, methodID).Custodian)
		})
	}
}

// TestCustodyMigration_PerRowOutcomes: the manifest is declared FACTS, so every
// way a line can fail to describe a real card gets its own verdict — and one
// bad line never stops the good ones.
func TestCustodyMigration_PerRowOutcomes(t *testing.T) {
	fx := newCustodyFixture(t)
	goodVault := "vault-" + uuid.NewString()[:8]
	goodMethod, _ := fx.seedPSPVaultedCard(t, goodVault)
	goodToken := "tok_" + uuid.NewString()[:12]

	// An instrument already at the custodian under a different token.
	otherVault := "vault-" + uuid.NewString()[:8]
	otherMethod, _ := fx.seedPSPVaultedCard(t, otherVault)
	_, err := fx.db.Pool().Exec(fx.ctx,
		`UPDATE openrails.payment_methods SET custodian = 'basis_theory', rail_method_ref = $2 WHERE id = $1`,
		otherMethod, "tok_already_"+uuid.NewString()[:8])
	require.NoError(t, err)

	// A third instrument, to be pointed at a token another instrument holds.
	conflictVault := "vault-" + uuid.NewString()[:8]
	conflictMethod, _ := fx.seedPSPVaultedCard(t, conflictVault)

	newCustomer := dbtest.EnsureCustomerIDPgx(fx.ctx, t, fx.db.Pool(), uuid.NewString())
	createdToken := "tok_" + uuid.NewString()[:12]
	createdVault := "vault-" + uuid.NewString()[:8]
	heldToken := fx.method(t, otherMethod).RailMethodRef

	exp := fx.export(
		custodymigration.ImportedToken{SourceRailCustomerRef: goodVault, Token: goodToken},
		custodymigration.ImportedToken{SourceRailCustomerRef: otherVault, Token: "tok_" + uuid.NewString()[:12]},
		custodymigration.ImportedToken{SourceRailCustomerRef: conflictVault, Token: heldToken},
		custodymigration.ImportedToken{SourceRailCustomerRef: "vault-nobody-" + uuid.NewString()[:8], Token: "tok_" + uuid.NewString()[:12]},
		custodymigration.ImportedToken{SourceRailCustomerRef: createdVault, Token: createdToken, Customer: &newCustomer, LastFour: "9999"},
		custodymigration.ImportedToken{SourceRailCustomerRef: "vault-x", Token: ""},
	)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.custody_migrations WHERE to_rail_method_ref = $1`, createdToken)
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.payment_methods WHERE rail_method_ref = $1`, createdToken)
	})

	res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.Equal(t, custodymigration.OutcomeRemapped, res.Rows[0].Outcome)
	require.Equal(t, custodymigration.OutcomeBlocked, res.Rows[1].Outcome)
	require.Equal(t, custodymigration.ReasonCustodyConflict, res.Rows[1].Reason)
	require.Equal(t, custodymigration.OutcomeBlocked, res.Rows[2].Outcome)
	require.Equal(t, custodymigration.ReasonTokenConflict, res.Rows[2].Reason)
	require.Equal(t, custodymigration.OutcomeUnmatched, res.Rows[3].Outcome)
	require.Equal(t, custodymigration.OutcomeCreated, res.Rows[4].Outcome)
	require.Equal(t, custodymigration.OutcomeBlocked, res.Rows[5].Outcome)
	require.Equal(t, custodymigration.ReasonMissingToken, res.Rows[5].Reason)

	require.Equal(t, models.CustodianBasisTheory, fx.method(t, goodMethod).Custodian)
	require.Equal(t, models.CustodianPSP, fx.method(t, conflictMethod).Custodian,
		"a token conflict never repoints two instruments at one card")

	// The created row carries its provenance: the vault entry it came out of.
	created := fx.method(t, *res.Rows[4].PaymentMethodID)
	require.Equal(t, models.CustodianBasisTheory, created.Custodian)
	require.Equal(t, createdToken, created.RailMethodRef)
	require.Equal(t, createdVault, created.RailCustomerRef)
	require.Equal(t, newCustomer, created.CustomerID)
	require.Equal(t, models.RebillDriverOpenRails, created.RebillDriver)
}

// TestCustodyMigration_DeclarationErrorsRefuseTheWholeRun: a declaration that
// cannot be true stops before the first instrument moves. Half a custody
// migration is the one outcome worth avoiding above all.
func TestCustodyMigration_DeclarationErrorsRefuseTheWholeRun(t *testing.T) {
	fx := newCustodyFixture(t)
	vaultID := "vault-" + uuid.NewString()[:8]
	methodID, _ := fx.seedPSPVaultedCard(t, vaultID)
	line := custodymigration.ImportedToken{SourceRailCustomerRef: vaultID, Token: "tok_" + uuid.NewString()[:12]}

	t.Run("no horizon", func(t *testing.T) {
		exp := fx.export(line)
		exp.ExportedAt = time.Time{}
		_, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		require.ErrorContains(t, err, "ExportedAt")
	})
	t.Run("undeclared custodian", func(t *testing.T) {
		exp := fx.export(line)
		exp.Custodian = "not-declared"
		_, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		require.ErrorContains(t, err, "declares no custodian")
	})
	t.Run("PSP charges through someone else's vault", func(t *testing.T) {
		exp := fx.export(line)
		exp.PSP = custodymigration.PSPRef{Rail: string(models.RailNMI), Environment: fx.env, AccountID: fx.oldPSP.AccountID}
		_, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		require.ErrorContains(t, err, "does not reference custodian")
	})
	t.Run("truncated manifest", func(t *testing.T) {
		exp := fx.export(line)
		n := 900
		exp.ExpectedTokens = &n
		_, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		require.ErrorContains(t, err, "ExpectedTokens")
	})
	t.Run("the same card claimed twice", func(t *testing.T) {
		exp := fx.export(line, line)
		res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		require.NoError(t, err)
		require.Equal(t, 2, res.Counts[custodymigration.OutcomeBlocked])
		require.Equal(t, custodymigration.ReasonDuplicateLine, res.Rows[0].Reason)
	})

	require.Equal(t, models.CustodianPSP, fx.method(t, methodID).Custodian, "no refusal moved anything")
	requireMigrationCount(t, fx, methodID, 0)
}

// TestRemappedInstrumentChargesThroughTheSurvivorGateway IS the deplatforming
// survival story, proved end to end: after the flip, the ordinary collection
// plane — the same store-armed resolver production uses — picks the custodian
// proxy transport off the INSTRUMENT, detokenizes the custodian's token, and
// presents the card to a DIFFERENT NMI gateway account with that gateway's own
// security key. Same card file, new processor, zero further changes.
func TestRemappedInstrumentChargesThroughTheSurvivorGateway(t *testing.T) {
	fx := newCustodyFixture(t)
	vaultID := "vault-" + uuid.NewString()[:8]
	methodID, _ := fx.seedPSPVaultedCard(t, vaultID)
	token := "tok_" + uuid.NewString()[:12]

	bt := newFakeBTProxy(t)
	builder := &money.MerchantCollectionAdapterBuilder{
		Config:      &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull},
		DB:          fx.db,
		MerchantsFn: func() *merchants.Service { return fx.merchants },
		Endpoints:   money.CollectionEndpoints{BTBaseURL: bt.srv.URL, NMIDirectPostURL: "https://survivor.example/api/transact.php"},
	}

	// BEFORE the flip the instrument is PSP-custodied, so the same resolver
	// arms the DIRECT NMI transport against the de-platforming gateway.
	adapter, ok, err := builder.ResolveCollectionAdapter(fx.ctx, fx.method(t, methodID))
	require.NoError(t, err)
	require.True(t, ok)
	require.IsType(t, &money.NMICollectionAdapter{}, adapter, "a PSP-vaulted card charges the processor directly")

	exp := fx.export(custodymigration.ImportedToken{SourceRailCustomerRef: vaultID, Token: token})
	res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.Equal(t, 1, res.Counts[custodymigration.OutcomeRemapped])

	remapped := fx.method(t, methodID)
	adapter, ok, err = builder.ResolveCollectionAdapter(fx.ctx, remapped)
	require.NoError(t, err)
	require.True(t, ok)
	require.IsType(t, &money.CustodianProxyCollectionAdapter{}, adapter,
		"the INSTRUMENT decides the transport (or#879) — nothing else changed")

	out, err := adapter.ChargeSavedMethod(fx.ctx, remapped, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		PaymentMethodID: methodID,
		AmountCents:     1999,
		Currency:        "USD",
		IdempotencyKey:  "invoice:or297:attempt:0",
		Description:     "post-remap collection",
	})
	require.NoError(t, err)
	require.False(t, out.Declined)
	require.Equal(t, bt.txnID, out.TransactionID)

	form := bt.lastForm(t)
	require.Contains(t, form.Get("ccnumber"), "token: "+token,
		"the card is detokenized from the CUSTODIAN, not from the dead processor vault")
	require.Equal(t, newGatewaySecurityKey, form.Get("security_key"),
		"the survivor gateway's own key charges it — this is the de-platforming escape")
	require.NotEqual(t, oldGatewaySecurityKey, form.Get("security_key"))
	require.Equal(t, "19.99", form.Get("amount"))
	require.Equal(t, "merchant", form.Get("initiated_by"))
	require.Equal(t, "used", form.Get("stored_credential_indicator"))
	require.Equal(t, "anchor-unsched-"+vaultID, form.Get("initial_transaction_id"),
		"the credential-on-file sequence survives the custody move intact")
	require.Equal(t, "https://survivor.example/api/transact.php", bt.lastDestination())
	require.Empty(t, form.Get("customer_vault_id"), "nothing addresses the stranded processor vault any more")
}

// ---------------------------------------------------------------- helpers

func seedChargeIntent(t *testing.T, fx *custodyFixture, subID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := fx.db.Pool().Exec(fx.ctx,
		`INSERT INTO openrails.rail_intents (id, merchant_id, rail, intent_type, subscription_id, idempotency_key, status, origin, origin_reason)
		 VALUES ($1, $2, 'nmi', 'manual_rebill', $3, $4, $5, 'system', 'or297 test')`,
		id, dbtest.TestMerchantID.UUID(), subID, "or297-"+uuid.NewString(), status)
	require.NoError(t, err)
	return id
}

func requireMigrationCount(t *testing.T, fx *custodyFixture, methodID uuid.UUID, want int) {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT count(*) FROM openrails.custody_migrations WHERE payment_method_id = $1`, methodID).Scan(&n))
	require.Equal(t, want, n)
}

func migrationRecord(t *testing.T, fx *custodyFixture, methodID uuid.UUID) gen.OpenrailsCustodyMigration {
	t.Helper()
	var rec gen.OpenrailsCustodyMigration
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT id, merchant_id, batch_id, payment_method_id, rail, from_custodian, from_custodian_id,
		        from_rail_customer_ref, from_rail_method_ref, from_psp_id, to_custodian, to_custodian_id,
		        to_rail_method_ref, to_psp_id, exported_at, outcome, reason, created_at
		   FROM openrails.custody_migrations WHERE payment_method_id = $1`, methodID).Scan(
		&rec.ID, &rec.MerchantID, &rec.BatchID, &rec.PaymentMethodID, &rec.Rail, &rec.FromCustodian, &rec.FromCustodianID,
		&rec.FromRailCustomerRef, &rec.FromRailMethodRef, &rec.FromPspID, &rec.ToCustodian, &rec.ToCustodianID,
		&rec.ToRailMethodRef, &rec.ToPspID, &rec.ExportedAt, &rec.Outcome, &rec.Reason, &rec.CreatedAt))
	return rec
}

// fakeBTProxy serves the one BT route a post-remap MIT touches: the ephemeral
// detokenizing proxy, answering with a canned NMI classic approval.
type fakeBTProxy struct {
	srv      *httptest.Server
	txnID    string
	form     atomic.Value // url.Values
	destHdr  atomic.Value // string
	proxyHit atomic.Int64
}

func newFakeBTProxy(t *testing.T) *fakeBTProxy {
	t.Helper()
	f := &fakeBTProxy{txnID: "txn" + uuid.NewString()[:8]}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/proxy" {
			f.proxyHit.Add(1)
			_ = r.ParseForm()
			f.form.Store(r.PostForm)
			f.destHdr.Store(r.Header.Get("BT-PROXY-URL"))
			w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
			fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=%s&response_code=100", f.txnID)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"title":"unhandled fake BT route %s %s","status":404}`, r.Method, r.URL.Path)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBTProxy) lastForm(t *testing.T) url.Values {
	t.Helper()
	v, _ := f.form.Load().(url.Values)
	require.NotNil(t, v, "no proxy call recorded")
	return v
}

func (f *fakeBTProxy) lastDestination() string {
	s, _ := f.destHdr.Load().(string)
	return s
}
