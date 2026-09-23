package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type catalogReferenceReaderFixture struct {
	data  map[solanago.PublicKey][]byte
	reads int
}

func (r *catalogReferenceReaderFixture) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	r.reads++
	return r.data[address], nil
}

type catalogReadOnlySubmitter struct {
	owner  solanago.PublicKey
	writes int
}

func (s *catalogReadOnlySubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return s.owner, nil
}
func (s *catalogReadOnlySubmitter) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	s.writes++
	return solanago.Signature{}, fmt.Errorf("provider writes forbidden")
}

func catalogReferencePlanFixture(t *testing.T) (context.Context, *recurring.PlanService, *catalogReferenceReaderFixture, *catalogReadOnlySubmitter, solanago.PublicKey) {
	t.Helper()
	const mintText = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mint := solanago.MustPublicKeyFromBase58(mintText)
	owner := solanago.NewWallet().PublicKey()
	hours := 720
	id := solanaPlanID("premium", "usd", 23000000, &hours, mintText)
	address, bump, err := subscriptions.DerivePlanPDA(owner, id)
	require.NoError(t, err)
	raw := make([]byte, subscriptions.PlanAccountSize)
	raw[0] = 1
	copy(raw[1:33], owner[:])
	raw[33] = bump
	raw[34] = subscriptions.PlanStatusActive
	binary.LittleEndian.PutUint64(raw[35:43], id)
	copy(raw[43:75], mint[:])
	binary.LittleEndian.PutUint64(raw[75:83], 23000000)
	binary.LittleEndian.PutUint64(raw[83:91], 720)
	binary.LittleEndian.PutUint64(raw[91:99], 1700000000)
	mintBytes, err := (mintReaderStub{mint: mintText, decimals: 6}).GetAccountData(context.Background(), mint)
	require.NoError(t, err)
	reader := &catalogReferenceReaderFixture{data: map[solanago.PublicKey][]byte{address: raw, mint: mintBytes}}
	submitter := &catalogReadOnlySubmitter{owner: owner}
	plan := recurring.NewPlanServiceWithReader(submitter, reader, "mainnet", map[string]config.TokenConfig{"USDC": {Mint: mintText}})
	return merchant.WithID(t.Context(), merchant.ID(uuid.New())), plan, reader, submitter, address
}

func TestCatalogReferencePreflightRejectsMissingAndMismatchedPlans(t *testing.T) {
	for _, response := range []string{`{"object":"plan","id":"known","plan_amount":"23.00","day_frequency":"0"}`, `{"object":"plan","id":"known","plan_amount":"23.00","day_frequency":"31"}`, `{"object":"plan","id":"known","plan_amount":"19.00","day_frequency":"30"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodGet, r.Method)
			fmt.Fprint(w, response)
		}))
		hours := 720
		_, err := verifyNMICatalogReference(nmiCatalogCtx(), newMobiusAdapterWithServer(t, server.URL), "mobius", CreatePriceRequest{Currency: "USD", UnitAmount: 23000000, AccessDurationHours: &hours, AutoRenew: true}, map[string]string{"plan_id": "known"})
		require.ErrorContains(t, err, "does not match")
		server.Close()
	}
	ctx, plan, reader, submitter, address := catalogReferencePlanFixture(t)
	hours := 720
	req := CreatePriceRequest{Currency: "USD", UnitAmount: 23000000, AccessDurationHours: &hours, AutoRenew: true}
	delete(reader.data, address)
	_, err := verifySolanaCatalogReference(ctx, plan, reader, "USDC", "premium", req, nil)
	require.ErrorContains(t, err, "separate provider workflow")
	require.Zero(t, submitter.writes)
}

func TestCatalogReferencePreflightRejectsForeignSolanaOwnerAndAmount(t *testing.T) {
	for _, kind := range []string{"owner", "amount", "period", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			ctx, plan, reader, submitter, address := catalogReferencePlanFixture(t)
			raw := reader.data[address]
			switch kind {
			case "owner":
				foreign := solanago.NewWallet().PublicKey()
				copy(raw[1:33], foreign[:])
			case "amount":
				binary.LittleEndian.PutUint64(raw[75:83], 19000000)
			case "period":
				binary.LittleEndian.PutUint64(raw[83:91], 744)
			case "inactive":
				raw[34] = subscriptions.PlanStatusSunset
			}
			hours := 720
			_, err := verifySolanaCatalogReference(ctx, plan, reader, "USDC", "premium", CreatePriceRequest{Currency: "USD", UnitAmount: 23000000, AccessDurationHours: &hours, AutoRenew: true}, map[string]string{solanaKeyPlanPDA: address.String()})
			require.Error(t, err)
			require.Zero(t, submitter.writes)
		})
	}
}

func TestCatalogReferencePreflightStripeNeverCreates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.True(t, strings.HasSuffix(r.URL.Path, "/prices/price_existing"))
		fmt.Fprint(w, `{"id":"price_existing","product":"prod_existing","unit_amount":2300,"currency":"usd","active":true,"recurring":{"interval":"month","interval_count":1}}`)
	}))
	t.Cleanup(server.Close)
	hours := 720
	adapter := newStripeAdapterWithServer(server.URL)
	req := CreatePriceRequest{Currency: "USD", UnitAmount: 23000000, AccessDurationHours: &hours, AutoRenew: true}
	out, err := verifyStripeCatalogReference(t.Context(), adapter, "", req, map[string]string{"price_id": "price_existing"})
	require.NoError(t, err)
	require.Equal(t, "prod_existing", out["product_id"])
	req.AutoRenew = false
	_, err = verifyStripeCatalogReference(t.Context(), adapter, "", req, map[string]string{"price_id": "price_existing"})
	require.ErrorContains(t, err, "recurring")
	_, err = verifyStripeCatalogReference(t.Context(), adapter, "", req, map[string]string{"lookup_key": "would-create"})
	require.ErrorContains(t, err, "existing Stripe price_id")
}

// This boundary sends only GETs. Pin the literal provider minor-unit readback
// against native catalog values, including zero-decimal JPY and values that
// cannot be represented exactly. Acceptance must never depend on rounding.
func TestCatalogReferencePreflightStripeCurrencyPrecision(t *testing.T) {
	for _, test := range []struct {
		name, currency      string
		native, remoteMinor int64
		reject              bool
	}{
		{name: "USD exact cents", currency: "USD", native: 19990000, remoteMinor: 1999},
		{name: "USD fractional cent refused", currency: "USD", native: 19995000, remoteMinor: 1999, reject: true},
		{name: "JPY whole yen", currency: "JPY", native: 5000000, remoteMinor: 500},
		{name: "JPY fractional yen refused", currency: "JPY", native: 5000001, remoteMinor: 500, reject: true},
		{name: "JPY wrong cent assumption refused", currency: "JPY", native: 5000000, remoteMinor: 50000, reject: true},
		{name: "integer above float precision", currency: "USD", native: 9007199254750000, remoteMinor: 900719925475},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("reference verifier attempted provider write %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if r.URL.Path != "/v1/prices/price_precision" {
					t.Errorf("unexpected provider path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fmt.Fprintf(w, `{"id":"price_precision","product":"prod_precision","unit_amount":%d,"currency":%q,"active":true}`, test.remoteMinor, strings.ToLower(test.currency))
			}))
			t.Cleanup(server.Close)
			_, err := verifyStripeCatalogReference(t.Context(), newStripeAdapterWithServer(server.URL), "", CreatePriceRequest{Currency: test.currency, UnitAmount: test.native}, map[string]string{"price_id": "price_precision"})
			if test.reject {
				require.Error(t, err, "fractional or mismatched wire amounts must never be accepted")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
