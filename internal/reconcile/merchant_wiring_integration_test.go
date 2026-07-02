//go:build integration

package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699 precedence rule: merchant-store first, boot-config rails fallback,
// conflict = store wins with a WARN. Exercised against a real Postgres
// (rail_merchant_accounts scope resolution) with a memory secret store.

func newWiringMerchant(t *testing.T, dbi *db.DB, slug string) merchant.ID {
	t.Helper()
	id := merchant.ID(uuid.New())
	_, err := dbi.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		id.UUID(), slug)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.rail_merchant_accounts WHERE merchant_id = $1`, id.UUID())
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = $1`, id.UUID())
	})
	return id
}

func newWiringService(t *testing.T, dbi *db.DB) *merchants.Service {
	t.Helper()
	svc, err := merchants.NewService(dbi.DataPool(), merchants.NewMemorySecretStore(), "live")
	require.NoError(t, err)
	return svc
}

func bootNMIRails(t *testing.T, accountID, securityKey string) (config.RailMerchantAccountSet, map[string]*nmi.NMIClient) {
	t.Helper()
	rails := config.RailMerchantAccountSet{
		"mobius": {
			Rail:      models.RailNMI,
			AccountID: accountID,
			NMI:       &config.NMIRailConfig{SecurityKey: securityKey},
		},
	}
	client, err := nmi.NewClient(accountID, &config.NMIProviderSettings{Name: accountID, SecurityKey: securityKey}, false)
	require.NoError(t, err)
	return rails, map[string]*nmi.NMIClient{accountID: client, "nmi": client}
}

func TestMerchantFetcherBuilder_StoreWinsOverBootWithWarn(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newWiringService(t, dbi)
	sfx := uuid.NewString()[:8]
	mid := newWiringMerchant(t, dbi, "wiring-store-"+sfx)

	storeKey := "store-key-" + sfx
	_, err := svc.UpsertPaymentProviderConfig(context.Background(), mid, "nmi", merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   "8811" + sfx,
		Credentials: map[string]string{"security_key": storeKey},
	})
	require.NoError(t, err)

	rails, clients := bootNMIRails(t, "8899"+sfx, "boot-key-"+sfx)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	armed := MerchantFetcherBuilder{
		Config:     &config.Config{Env: "dev"},
		Rails:      rails,
		Merchants:  svc,
		DB:         dbi,
		NMIClients: clients,
	}.Build(context.Background(), mid)

	prober, ok := armed.Probers[ProviderNMI].(*NMISubscriptionProber)
	require.True(t, ok, "NMI prober armed")
	assert.Equal(t, storeKey, prober.Client.SecurityKey, "store credential wins over boot config")
	require.Contains(t, armed.Fetchers, ProviderNMI)

	conflictWarned := false
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "store wins") && e.Data["rail"] == "nmi" {
			conflictWarned = true
		}
	}
	assert.True(t, conflictWarned, "differing planes log the store-wins WARN")
}

func TestMerchantFetcherBuilder_BootFallbackWhenNoStoreRow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newWiringService(t, dbi)
	sfx := uuid.NewString()[:8]
	// Merchant declares NO rail accounts at all.
	mid := newWiringMerchant(t, dbi, "wiring-boot-"+sfx)

	bootKey := "boot-only-key-" + sfx
	rails, clients := bootNMIRails(t, "8822"+sfx, bootKey)

	armed := MerchantFetcherBuilder{
		Config:     &config.Config{Env: "dev"},
		Rails:      rails,
		Merchants:  svc,
		DB:         dbi,
		NMIClients: clients,
	}.Build(context.Background(), mid)

	prober, ok := armed.Probers[ProviderNMI].(*NMISubscriptionProber)
	require.True(t, ok, "boot-config fallback arms the NMI prober")
	assert.Equal(t, bootKey, prober.Client.SecurityKey)
	require.Contains(t, armed.Fetchers, ProviderNMI)
}

func TestMerchantFetcherBuilder_DeclaredAccountNeverFallsBackAcrossPlanes(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newWiringService(t, dbi)
	sfx := uuid.NewString()[:8]
	mid := newWiringMerchant(t, dbi, "wiring-closed-"+sfx)

	// Account row declared, but the security_key secret was never seeded.
	_, err := svc.UpsertPaymentProviderConfig(context.Background(), mid, "nmi", merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   "8833" + sfx,
	})
	require.NoError(t, err)

	rails, clients := bootNMIRails(t, "8844"+sfx, "boot-key2-"+sfx)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	armed := MerchantFetcherBuilder{
		Config:     &config.Config{Env: "dev"},
		Rails:      rails,
		Merchants:  svc,
		DB:         dbi,
		NMIClients: clients,
	}.Build(context.Background(), mid)

	assert.NotContains(t, armed.Fetchers, ProviderNMI, "declared account with missing secret fails closed — no boot fallback")
	assert.NotContains(t, armed.Probers, ProviderNMI)

	warned := false
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "merchant secret missing") &&
			e.Data["merchant_id"] == mid.String() && e.Data["rail"] == "nmi" {
			warned = true
		}
	}
	assert.True(t, warned, "missing secret logs the naming WARN")
}
