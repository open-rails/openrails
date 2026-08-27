package paymentmethods

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

type fakePaymentMethodStore struct {
	created []*models.PaymentMethod
}

func TestNMINamePartsPrefersCanonicalFullNameAndAcceptsMononym(t *testing.T) {
	first, last := nmiNameParts("Stale", "Alias", "María José Carreño Quiñones")
	require.Equal(t, "María", first)
	require.Equal(t, "José Carreño Quiñones", last)

	first, last = nmiNameParts("", "", "Prince")
	require.Equal(t, "Prince", first)
	require.Empty(t, last)

	first, last = nmiNameParts("María de", "la Vega", "")
	require.Equal(t, "María de", first)
	require.Equal(t, "la Vega", last)
}

func (f *fakePaymentMethodStore) Create(_ context.Context, method *models.PaymentMethod) error {
	f.created = append(f.created, method)
	return nil
}

func (f *fakePaymentMethodStore) Update(_ context.Context, _ *models.PaymentMethod) error {
	return nil
}

func (f *fakePaymentMethodStore) Delete(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (f *fakePaymentMethodStore) GetByUserID(_ context.Context, _ string) ([]*models.PaymentMethod, error) {
	return nil, nil
}

type vaultUnavailableSecretStore struct{}

func (vaultUnavailableSecretStore) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{}, merchants.ErrSecretBackendUnavailable
}

func TestPreparePaymentMethodUpdateNormalizesTokenizedMetadata(t *testing.T) {
	token := "  token-1  "
	lastFour := "4242"
	cardType := "Visa"
	expiryDate := "12-2030"
	req := &UpdatePaymentMethodRequest{
		PaymentToken: &token,
		LastFour:     &lastFour,
		CardType:     &cardType,
		ExpiryDate:   &expiryDate,
	}

	require.NoError(t, preparePaymentMethodUpdate(req))
	require.Equal(t, "token-1", *req.PaymentToken)
	require.Equal(t, "4242", *req.LastFour)
	require.Equal(t, "Visa", *req.CardType)
	require.Equal(t, "12/30", *req.ExpiryDate)
}

func TestPreparePaymentMethodUpdateRequiresVerifiableMetadata(t *testing.T) {
	token := "token-1"
	err := preparePaymentMethodUpdate(&UpdatePaymentMethodRequest{PaymentToken: &token})
	require.EqualError(t, err, "last_four, card_type, and expiry_date are required from the tokenization response")
}

type fakePaymentMethodUpdateExecutor struct {
	out PaymentMethodUpdateOutcome
	err error
}

func (f fakePaymentMethodUpdateExecutor) ExecutePaymentMethodUpdate(context.Context, *models.PaymentMethod, *UpdatePaymentMethodRequest) (PaymentMethodUpdateOutcome, error) {
	return f.out, f.err
}

func TestUpdatePaymentMethodMapsDurableOutcome(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("nmi", "test", "mobius-account", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)
	pm := &models.PaymentMethod{ID: uuid.New(), CustomerID: uuid.New(), Rail: models.RailNMI, RailCustomerRef: "vault-1"}
	token, lastFour, cardType, expiry := "token-1", "4242", "Visa", "12/30"
	request := func() *UpdatePaymentMethodRequest {
		return &UpdatePaymentMethodRequest{PaymentToken: &token, LastFour: &lastFour, CardType: &cardType, ExpiryDate: &expiry}
	}

	tests := []struct {
		name    string
		out     PaymentMethodUpdateOutcome
		want    *models.PaymentMethod
		wantErr error
		failed  bool
	}{
		{name: "confirmed", out: PaymentMethodUpdateOutcome{Done: true, Method: pm}, want: pm},
		{name: "processing", out: PaymentMethodUpdateOutcome{}, wantErr: ErrPaymentMethodUpdateProcessing},
		{name: "fresh token required", out: PaymentMethodUpdateOutcome{Terminal: true, Retokenize: true}, wantErr: ErrPaymentMethodRetokenize},
		{name: "terminal conflict", out: PaymentMethodUpdateOutcome{Terminal: true, Reason: "provider state conflict"}, failed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &RailPaymentMethodService{
				Config:          vaultTestConfig(true),
				MerchantSecrets: store,
				ProviderSecrets: vaultStaticProviderSecretResolver{rail: "nmi", environment: "test", accountID: "mobius-account"},
				UpdateIntents:   fakePaymentMethodUpdateExecutor{out: tt.out},
			}
			got, err := svc.UpdatePaymentMethod(ctx, pm, request())
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.failed {
				var terminal *PaymentMethodUpdateFailedError
				require.ErrorAs(t, err, &terminal)
				require.Equal(t, "provider state conflict", terminal.Reason)
				return
			}
			require.NoError(t, err)
			require.Same(t, tt.want, got)
		})
	}
}

func TestCreateVaultUsesMerchantSecretMobiusKeyWithoutStaticClient(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	store := merchants.NewMemorySecretStore()
	secretName, err := merchants.PSPSecretName("nmi", "live", "mobius-account", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, "merchant-mobius-key")
	require.NoError(t, err)

	type v5CreateSeen struct {
		auth string
		body map[string]any
	}
	seen := make(chan v5CreateSeen, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/customers", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		seen <- v5CreateSeen{auth: r.Header.Get("Authorization"), body: body}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, `{"object":"customer","id":"vault_123","billing":[{"object":"billing","id":"billing_456","priority":1}]}`)
	}))
	defer server.Close()

	pms := &fakePaymentMethodStore{}
	svc := &RailPaymentMethodService{
		PaymentMethodService: pms,
		MerchantSecrets:      store,
		ProviderSecrets:      vaultStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"},
		Config:               vaultTestConfig(true),
		newNMIClient: func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error) {
			client, err := nmi.NewClient(provider, cfg, testMode)
			if err != nil {
				return nil, err
			}
			client.DirectPostURL = server.URL
			client.V5BaseURL = server.URL
			return client, nil
		},
	}

	pm, err := svc.CreatePaymentMethod(ctx, "11111111-1111-1111-1111-111111111111", &CreatePaymentMethodRequest{
		Provider:     "nmi",
		PaymentToken: "provider-token",
		NameOnCard:   "Ada Lovelace",
		LastFour:     "xx4242",
		CardType:     "Visa",
		ExpiryDate:   "12/30",
		Metadata: map[string]any{
			"name_on_card": "Ada Lovelace",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "vault_123", pm.RailCustomerRef)
	require.Equal(t, "billing_456", pm.RailMethodRef, "billing id recorded verbatim (#682)")
	require.Equal(t, models.RebillDriverProvider, pm.RebillDriver, "native vaults stay provider-billed regardless of billing id")
	require.Len(t, pms.created, 1)

	got := <-seen
	// The merchant-secret key is the ENTIRE Authorization header (v5).
	require.Equal(t, "merchant-mobius-key", got.auth)
	billing, ok := got.body["billing"].(map[string]any)
	require.True(t, ok, "v5 create-customer body must carry a billing object: %v", got.body)
	paymentDetails, ok := billing["payment_details"].(map[string]any)
	require.True(t, ok, "billing must carry payment_details: %v", billing)
	require.Equal(t, "provider-token", paymentDetails["payment_token"])
	require.Equal(t, "Ada", billing["first_name"])
	require.Equal(t, "Lovelace", billing["last_name"])
	require.Equal(t, "4242", *pm.LastFour)
	require.Equal(t, "Visa", *pm.CardType)
	require.Equal(t, "12/30", *pm.ExpiryDate)
	require.Equal(t, "Ada Lovelace", pm.Metadata["name_on_card"])

	var timingFields map[string]any
	for _, entry := range hook.AllEntries() {
		if entry.Message == "Payment method create timing" {
			timingFields = entry.Data
			break
		}
	}
	require.NotNil(t, timingFields)
	require.Equal(t, "payment_method_create", timingFields["operation"])
	require.Equal(t, "nmi", timingFields["provider"])
	require.Equal(t, "success", timingFields["outcome"])
	require.GreaterOrEqual(t, timingFields["provider_duration_ms"], int64(5))
	require.GreaterOrEqual(t, timingFields["database_duration_ms"], int64(0))
	require.GreaterOrEqual(t, timingFields["total_duration_ms"], timingFields["provider_duration_ms"])
}

// #788: the boot-config plane is gone — a vault create with no armed rail
// account fails closed instead of falling back to a static client.
func TestVaultWithoutArmedRailAccountFailsClosed(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &RailPaymentMethodService{
		Config: vaultTestConfig(true),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing client")
}

func TestVaultPSPResolverMissingMobiusSecretDoesNotUseStaticClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &RailPaymentMethodService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
		ProviderSecrets: vaultMissingProviderSecretResolver{},
		Config:          vaultTestConfig(true),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing scoped merchant NMI PSP")
}

func TestVaultMissingMerchantSecretAndNoStaticClientReturnsMissingClient(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &RailPaymentMethodService{
		MerchantSecrets: merchants.NewMemorySecretStore(),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing client")
}

func TestVaultFailsClosedWhenMerchantSecretBackendUnavailable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := &RailPaymentMethodService{
		MerchantSecrets: vaultUnavailableSecretStore{},
		ProviderSecrets: vaultStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"},
		Config:          vaultTestConfig(true),
	}

	_, _, err := svc.resolveNMIClient(ctx, "nmi")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretBackendUnavailable), "err = %v", err)
}

func TestVaultMerchantSecretResolutionIsMerchantScoped(t *testing.T) {
	merchantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	merchantB := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))
	store := merchants.NewMemorySecretStore()
	nameA, err := merchants.PSPSecretName("nmi", "live", "mobius-a", "security_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantA, nameA, "merchant-a-key")
	require.NoError(t, err)
	nameB, err := merchants.PSPSecretName("nmi", "live", "mobius-b", "security_key")
	require.NoError(t, err)
	_, err = store.Put(context.Background(), merchantB, nameB, "merchant-b-key")
	require.NoError(t, err)

	svc := &RailPaymentMethodService{
		MerchantSecrets: store,
		ProviderSecrets: vaultPerMerchantProviderSecretResolver{
			merchantA: "mobius-a",
			merchantB: "mobius-b",
		},
		Config: vaultTestConfig(true),
	}

	clientA, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantA), "nmi")
	require.NoError(t, err)
	clientB, _, err := svc.resolveNMIClient(merchant.WithID(context.Background(), merchantB), "nmi")
	require.NoError(t, err)
	require.Equal(t, "merchant-a-key", clientA.SecurityKey)
	require.Equal(t, "merchant-b-key", clientB.SecurityKey)
}

type vaultStaticProviderSecretResolver struct {
	rail        string
	environment string
	accountID   string
}

func (r vaultStaticProviderSecretResolver) ActivePSPSecretName(_ context.Context, _ merchant.ID, _, _, key string) (string, bool, error) {
	name, err := merchants.PSPSecretName(r.rail, r.environment, r.accountID, key)
	return name, err == nil, err
}

func (r vaultStaticProviderSecretResolver) ActivePSPScope(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{
		Rail:        r.rail,
		Environment: r.environment,
		AccountID:   r.accountID,
	}, true, nil
}

type vaultMissingProviderSecretResolver struct{}

func (vaultMissingProviderSecretResolver) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (vaultMissingProviderSecretResolver) ActivePSPScope(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

type vaultPerMerchantProviderSecretResolver map[merchant.ID]string

func (r vaultPerMerchantProviderSecretResolver) ActivePSPSecretName(_ context.Context, id merchant.ID, _, _, key string) (string, bool, error) {
	accountID := r[id]
	if accountID == "" {
		return "", false, nil
	}
	name, err := merchants.PSPSecretName("nmi", "live", accountID, key)
	return name, err == nil, err
}

func (r vaultPerMerchantProviderSecretResolver) ActivePSPScope(_ context.Context, id merchant.ID, _, _ string) (merchants.PSPScope, bool, error) {
	accountID := r[id]
	if accountID == "" {
		return merchants.PSPScope{}, false, nil
	}
	return merchants.PSPScope{Rail: "nmi", Environment: "live", AccountID: accountID}, true, nil
}

func vaultTestConfig(testMode bool) *config.Config {
	posture := config.CredentialPostureLive
	if testMode {
		posture = config.CredentialPostureSandbox
	}
	return &config.Config{
		ProviderWriteMode: config.ProviderWriteModeFull,
		TestMode:          posture,
	}
}

// --- DeletePaymentMethod durable-intent producer branching (#674 tail) ---

type deleteVaultNoSubs struct{}

func (deleteVaultNoSubs) GetPaginatedByUserID(context.Context, string, int, int) ([]models.Subscription, int, error) {
	return nil, 0, nil
}

type deleteVaultSubscriptions struct {
	subscriptions []models.Subscription
}

func (f deleteVaultSubscriptions) GetPaginatedByUserID(context.Context, string, int, int) ([]models.Subscription, int, error) {
	return f.subscriptions, len(f.subscriptions), nil
}

type fakeVaultDeleteExecutor struct {
	out    PaymentMethodDeleteOutcome
	err    error
	called int
}

func (f *fakeVaultDeleteExecutor) ExecutePaymentMethodDelete(context.Context, *models.PaymentMethod) (PaymentMethodDeleteOutcome, error) {
	f.called++
	return f.out, f.err
}

func deleteVaultTestService(exec PaymentMethodDeleteExecutor) (*RailPaymentMethodService, *models.PaymentMethod) {
	// #788: the NMI client arms from the scoped merchant secret store.
	store := merchants.NewMemorySecretStore()
	name, err := merchants.PSPSecretName("nmi", "live", "mobius-account", "security_key")
	if err != nil {
		panic(err)
	}
	if _, err := store.Put(context.Background(), dbtest.TestMerchantID, name, "k"); err != nil {
		panic(err)
	}
	svc := &RailPaymentMethodService{
		SubscriptionService: deleteVaultNoSubs{},
		Config:              vaultTestConfig(true),
		DeleteIntents:       exec,
		MerchantSecrets:     store,
		ProviderSecrets:     vaultStaticProviderSecretResolver{rail: "nmi", environment: "live", accountID: "mobius-account"},
	}
	pm := &models.PaymentMethod{
		ID:              uuid.New(),
		CustomerID:      uuid.New(),
		Rail:            models.RailNMI,
		RailCustomerRef: "vault-1",
		RailMethodRef:   "bill-1",
	}
	return svc, pm
}

// The producer maps the durable intent's post-execution state onto the caller
// contract: Done ⇒ nil, Terminal ⇒ error with the reason, anything still
// resolving ⇒ ErrPaymentMethodDeleteProcessing (the ledger finishes out-of-band).
func TestDeleteVaultBranchesOnIntentOutcome(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	exec := &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
	svc, pm := deleteVaultTestService(exec)
	require.NoError(t, svc.DeletePaymentMethod(ctx, pm))
	require.Equal(t, 1, exec.called)

	exec = &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{Terminal: true, Reason: "shared vault, no billing id"}}
	svc, pm = deleteVaultTestService(exec)
	err := svc.DeletePaymentMethod(ctx, pm)
	var terminal *PaymentMethodDeleteFailedError
	require.ErrorAs(t, err, &terminal)
	require.Equal(t, "shared vault, no billing id", terminal.Reason)

	exec = &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{Reason: "vault delete outcome unknown"}}
	svc, pm = deleteVaultTestService(exec)
	require.ErrorIs(t, svc.DeletePaymentMethod(ctx, pm), ErrPaymentMethodDeleteProcessing)

	exec = &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{InUse: true, Reason: "back in use"}}
	svc, pm = deleteVaultTestService(exec)
	require.ErrorIs(t, svc.DeletePaymentMethod(ctx, pm), ErrPaymentMethodInUse)
}

func TestDeletePaymentMethodBlocksEveryLiveSubscriptionStatus(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	for _, status := range []models.SubscriptionStatus{
		models.StatusActive,
		models.StatusPending,
		models.StatusPastDue,
	} {
		t.Run(string(status), func(t *testing.T) {
			exec := &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
			svc, pm := deleteVaultTestService(exec)
			svc.SubscriptionService = deleteVaultSubscriptions{subscriptions: []models.Subscription{{
				ID:              uuid.New(),
				CustomerID:      pm.CustomerID,
				PaymentMethodID: &pm.ID,
				Status:          status,
			}}}

			err := svc.DeletePaymentMethod(ctx, pm)
			require.ErrorIs(t, err, ErrPaymentMethodInUse)
			require.Zero(t, exec.called, "a live subscription must block before the delete intent is posted")
		})
	}
}

func TestDeletePaymentMethodRejectsProviderManagedRails(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	for _, rail := range []models.Rail{models.RailStripe, models.RailCCBill, models.RailSolana} {
		t.Run(string(rail), func(t *testing.T) {
			exec := &fakeVaultDeleteExecutor{out: PaymentMethodDeleteOutcome{Done: true}}
			svc, pm := deleteVaultTestService(exec)
			pm.Rail = rail

			err := svc.DeletePaymentMethod(ctx, pm)
			require.ErrorIs(t, err, ErrPaymentMethodsUnsupportedOnRail)
			require.Zero(t, exec.called, "provider-managed methods must never reach the NMI delete intent")
		})
	}
}

func TestDeleteVaultRequiresIntentExecutor(t *testing.T) {
	svc, pm := deleteVaultTestService(nil)
	err := svc.DeletePaymentMethod(merchant.WithID(context.Background(), dbtest.TestMerchantID), pm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not wired")
}
