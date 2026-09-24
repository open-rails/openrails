package paymentmethods

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type pspResolver map[merchant.ID]string // merchant -> armed NMI account id

func (r pspResolver) ActivePSPSecretName(_ context.Context, id merchant.ID, _, _, key string) (string, bool, error) {
	if r[id] == "" {
		return "", false, nil
	}
	name, err := merchants.PSPSecretName("nmi", "live", r[id], key)
	return name, err == nil, err
}

func (r pspResolver) ActivePSPScope(_ context.Context, id merchant.ID, _, _ string) (merchants.PSPScope, bool, error) {
	if r[id] == "" {
		return merchants.PSPScope{}, false, nil
	}
	pspID, _, _, _ := merchants.PSPNaturalKey("nmi", "live", r[id])
	return merchants.PSPScope{ID: pspID, Rail: "nmi", Environment: "live", AccountID: r[id]}, true, nil
}

type unavailableSecrets struct{}

func (unavailableSecrets) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{}, merchants.ErrSecretBackendUnavailable
}

func testConfig() *config.Config {
	return &config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}
}

func secretsFor(t *testing.T, keys map[merchant.ID][2]string) merchants.MerchantSecretStore {
	t.Helper()
	store := merchants.NewMemorySecretStore()
	for id, kv := range keys {
		name, err := merchants.PSPSecretName("nmi", "live", kv[0], "security_key")
		require.NoError(t, err)
		_, err = store.Put(context.Background(), id, name, kv[1])
		require.NoError(t, err)
	}
	return store
}

func TestNMINameParts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ first, last, full, wantFirst, wantLast string }{
		{"Stale", "Alias", "María José Carreño Quiñones", "María", "José Carreño Quiñones"},
		{"", "", "Prince", "Prince", ""},
		{"María de", "la Vega", "", "María de", "la Vega"},
	} {
		first, last := nmiNameParts(tc.first, tc.last, tc.full)
		require.Equal(t, tc.wantFirst, first, tc.full)
		require.Equal(t, tc.wantLast, last, tc.full)
	}
}

func TestPreparePaymentMethodUpdate(t *testing.T) {
	t.Parallel()
	token, last4, brand, expiry := "  token-1  ", "4242", "Visa", "12-2030"
	req := &UpdatePaymentMethodRequest{PaymentToken: &token, LastFour: &last4, CardType: &brand, ExpiryDate: &expiry}
	require.NoError(t, preparePaymentMethodUpdate(req))
	require.Equal(t, "token-1", *req.PaymentToken)
	require.Equal(t, "12/30", *req.ExpiryDate)

	bare := "token-1"
	require.EqualError(t, preparePaymentMethodUpdate(&UpdatePaymentMethodRequest{PaymentToken: &bare}), "last_four and expiry_date are required from the tokenization response")
}

// #788: the NMI client arms only from the ctx merchant's scoped secret; no static fallback exists.
func TestResolveNMIClientIsMerchantScopedAndFailsClosed(t *testing.T) {
	t.Parallel()
	a := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	b := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	svc := &RailPaymentMethodService{
		MerchantSecrets: secretsFor(t, map[merchant.ID][2]string{a: {"mobius-a", "merchant-a-key"}, b: {"mobius-b", "merchant-b-key"}}),
		ProviderSecrets: pspResolver{a: "mobius-a", b: "mobius-b"},
		Config:          testConfig(),
	}
	clientA, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), a), "nmi")
	require.NoError(t, err)
	clientB, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), b), "nmi")
	require.NoError(t, err)
	require.Equal(t, "merchant-a-key", clientA.SecurityKey)
	require.Equal(t, "merchant-b-key", clientB.SecurityKey)

	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	for name, tc := range map[string]struct {
		svc     *RailPaymentMethodService
		errText string
		errIs   error
	}{
		"no armed account":    {&RailPaymentMethodService{Config: testConfig()}, "missing client", nil},
		"no secret store psp": {&RailPaymentMethodService{MerchantSecrets: merchants.NewMemorySecretStore()}, "missing client", nil},
		"psp not armed":       {&RailPaymentMethodService{MerchantSecrets: merchants.NewMemorySecretStore(), ProviderSecrets: pspResolver{}, Config: testConfig()}, "missing scoped merchant NMI PSP", nil},
		"backend unavailable": {&RailPaymentMethodService{MerchantSecrets: unavailableSecrets{}, ProviderSecrets: pspResolver{dbtest.TestMerchantID: "mobius"}, Config: testConfig()}, "", merchants.ErrSecretBackendUnavailable},
	} {
		_, _, err := tc.svc.resolveNMIClient(ctx, "nmi")
		require.Error(t, err, name)
		if tc.errIs != nil {
			require.ErrorIs(t, err, tc.errIs, name)
		} else {
			require.ErrorContains(t, err, tc.errText, name)
		}
	}
}

type updateExecutor struct{ out PaymentMethodUpdateOutcome }

func (f updateExecutor) ExecutePaymentMethodUpdate(context.Context, *models.PaymentMethod, *UpdatePaymentMethodRequest) (PaymentMethodUpdateOutcome, error) {
	return f.out, nil
}

type deleteExecutor struct {
	out    PaymentMethodDeleteOutcome
	called int
}

func (f *deleteExecutor) ExecutePaymentMethodDelete(context.Context, *models.PaymentMethod) (PaymentMethodDeleteOutcome, error) {
	f.called++
	return f.out, nil
}

type subscriptionLister []models.Subscription

func (s subscriptionLister) GetPaginatedByUserID(context.Context, string, int, int) ([]models.Subscription, int, error) {
	return s, len(s), nil
}

func vaultService(t *testing.T, del PaymentMethodDeleteExecutor) (*RailPaymentMethodService, *models.PaymentMethod) {
	svc := &RailPaymentMethodService{
		SubscriptionService: subscriptionLister(nil),
		Config:              testConfig(),
		DeleteIntents:       del,
		MerchantSecrets:     secretsFor(t, map[merchant.ID][2]string{dbtest.TestMerchantID: {"mobius-account", "k"}}),
		ProviderSecrets:     pspResolver{dbtest.TestMerchantID: "mobius-account"},
	}
	pm := &models.PaymentMethod{Custodian: models.CustodianPSP, ID: uuid.New(), CustomerID: uuid.New(), Rail: models.RailNMI, RailCustomerRef: "vault-1", RailMethodRef: "bill-1"}
	return svc, pm
}

// The durable intent's state maps onto the caller contract; unresolved work is "processing", never success.
func TestPaymentMethodIntentOutcomes(t *testing.T) {
	t.Parallel()
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	token, last4, brand, expiry := "token-1", "4242", "Visa", "12/30"
	update := func(out PaymentMethodUpdateOutcome) (*models.PaymentMethod, *models.PaymentMethod, error) {
		svc, pm := vaultService(t, nil)
		svc.UpdateIntents = updateExecutor{out: out}
		if out.Done {
			out.Method = pm
			svc.UpdateIntents = updateExecutor{out: out}
		}
		got, err := svc.UpdatePaymentMethod(ctx, pm, &UpdatePaymentMethodRequest{PaymentToken: &token, LastFour: &last4, CardType: &brand, ExpiryDate: &expiry})
		return pm, got, err
	}
	pm, got, err := update(PaymentMethodUpdateOutcome{Done: true})
	require.NoError(t, err)
	require.Same(t, pm, got)
	_, _, err = update(PaymentMethodUpdateOutcome{})
	require.ErrorIs(t, err, ErrPaymentMethodUpdateProcessing)
	_, _, err = update(PaymentMethodUpdateOutcome{Terminal: true, Retokenize: true})
	require.ErrorIs(t, err, ErrPaymentMethodRetokenize)
	_, _, err = update(PaymentMethodUpdateOutcome{Terminal: true, Reason: "provider state conflict"})
	var updateFailed *PaymentMethodUpdateFailedError
	require.ErrorAs(t, err, &updateFailed)
	require.Equal(t, "provider state conflict", updateFailed.Reason)

	for _, tc := range []struct {
		out   PaymentMethodDeleteOutcome
		check func(error)
	}{
		{PaymentMethodDeleteOutcome{Done: true}, func(err error) { require.NoError(t, err) }},
		{PaymentMethodDeleteOutcome{Reason: "vault delete outcome unknown"}, func(err error) { require.ErrorIs(t, err, ErrPaymentMethodDeleteProcessing) }},
		{PaymentMethodDeleteOutcome{InUse: true, Reason: "back in use"}, func(err error) { require.ErrorIs(t, err, ErrPaymentMethodInUse) }},
		{PaymentMethodDeleteOutcome{Terminal: true, Reason: "shared vault, no billing id"}, func(err error) {
			var failed *PaymentMethodDeleteFailedError
			require.ErrorAs(t, err, &failed)
			require.Equal(t, "shared vault, no billing id", failed.Reason)
		}},
	} {
		exec := &deleteExecutor{out: tc.out}
		svc, pm := vaultService(t, exec)
		tc.check(svc.DeletePaymentMethod(ctx, pm))
		require.Equal(t, 1, exec.called)
	}

	svc, pm := vaultService(t, nil)
	require.ErrorContains(t, svc.DeletePaymentMethod(ctx, pm), "not wired")
}

// Refusals that must happen before any delete intent is posted.
func TestDeletePaymentMethodGuards(t *testing.T) {
	t.Parallel()
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	for _, status := range []models.SubscriptionStatus{models.StatusActive, models.StatusPending, models.StatusPastDue} {
		exec := &deleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
		svc, pm := vaultService(t, exec)
		svc.SubscriptionService = subscriptionLister{{ID: uuid.New(), CustomerID: pm.CustomerID, PaymentMethodID: &pm.ID, Status: status}}
		require.ErrorIs(t, svc.DeletePaymentMethod(ctx, pm), ErrPaymentMethodInUse, status)
		require.Zero(t, exec.called, status)
	}
	for _, rail := range []models.Rail{models.RailStripe, models.RailCCBill, models.RailSolana} {
		exec := &deleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
		svc, pm := vaultService(t, exec)
		pm.Rail = rail
		require.ErrorIs(t, svc.DeletePaymentMethod(ctx, pm), ErrPaymentMethodsUnsupportedOnRail, rail)
		require.Zero(t, exec.called, rail)
	}
	// Custodian-held cards never reach native NMI vault operations; HyperSwitch deletes through its own intent.
	for _, kind := range []string{models.CustodianBasisTheory, models.CustodianHyperSwitch, ""} {
		exec := &deleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
		svc, pm := vaultService(t, exec)
		pm.Custodian = kind
		if kind == models.CustodianHyperSwitch {
			require.NoError(t, svc.DeletePaymentMethod(ctx, pm))
			require.Equal(t, 1, exec.called)
		} else {
			require.ErrorIs(t, svc.DeletePaymentMethod(ctx, pm), ErrPaymentMethodCustodianUnsupported, kind)
			require.Zero(t, exec.called, kind)
		}
		require.ErrorIs(t, svc.CleanupPaymentMethodBestEffort(ctx, pm), ErrPaymentMethodCustodianUnsupported, kind)
		_, err := svc.ResolveClientForPaymentMethod(ctx, pm)
		require.ErrorIs(t, err, ErrPaymentMethodCustodianUnsupported, kind)
		_, err = svc.UpdatePaymentMethod(ctx, pm, &UpdatePaymentMethodRequest{})
		require.ErrorIs(t, err, ErrPaymentMethodCustodianUnsupported, kind)
	}
}
