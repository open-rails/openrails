package rails

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// Unkeyed literals force every FIELD at compile time; this forces every RAIL and the fields' shape.
func TestRegistryCompleteness(t *testing.T) {
	t.Parallel()
	enum := []models.Rail{models.RailNMI, models.RailCCBill, models.RailStripe, models.RailSolana}
	require.Len(t, All(), len(enum))
	for i, rail := range enum {
		d, ok := Lookup(rail)
		require.True(t, ok, rail)
		require.Equal(t, rail, d.Rail)
		require.Equal(t, rail, All()[i].Rail, "declaration order is load-bearing")
		require.NotEmpty(t, strings.TrimSpace(d.DisplayName), rail)
		require.NotNil(t, d.AutoBilled, rail)
		require.NotNil(t, d.CancelMode, rail)
		require.Empty(t, d.CancelPortalURL, "no rail has a consumer portal since #696")
		for _, k := range d.CredentialKeys {
			require.Equal(t, strings.ToLower(strings.TrimSpace(k.Name)), k.Name, rail)
		}
	}
}

// Pins the per-rail facts, so a descriptor edit that flips behaviour fails here.
func TestRegistryPinnedFacts(t *testing.T) {
	t.Parallel()
	delegated := &models.Subscription{CollectionPolicy: models.CollectionPolicyNMISchedule}
	for _, c := range []struct {
		rail                                                                    models.Rail
		remoteCustomer, chargeSaved, dunning, psps, remoteDelete, trial, pmCRUD bool
		autoBilledNil, autoBilledDelegated                                      bool
		merchantKeys                                                            []string
		activeCancel                                                            CancelMode
	}{
		{models.RailNMI, false, true, true, true, true, false, true, true, false,
			[]string{"security_key", "webhook_signing_secret", "webhook_signing_secret_previous"}, CancelModeDestructive},
		{models.RailCCBill, false, false, false, true, false, true, false, true, true,
			[]string{"salt", "datalink_username", "datalink_password"}, CancelModeDestructive},
		{models.RailStripe, true, true, false, true, false, true, false, false, false,
			[]string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous"}, CancelModeReversible},
		{models.RailSolana, false, false, true, true, false, false, false, false, false, nil, CancelModeReversible},
	} {
		d, _ := Lookup(c.rail)
		require.Equal(t, c.remoteCustomer, HasRemoteCustomer(c.rail), c.rail)
		require.Equal(t, c.chargeSaved, d.SupportsChargeSavedMethod, c.rail)
		require.Equal(t, c.dunning, d.OpenRailsDrivenDunning, c.rail)
		require.Equal(t, c.psps, SupportsPSPs(c.rail), c.rail)
		require.Equal(t, c.remoteDelete, RemoteDeleteOnTerminalCancel(c.rail), c.rail)
		require.Equal(t, c.trial, SupportsCatalogTrial(c.rail), c.rail)
		require.Equal(t, c.pmCRUD, SupportsPaymentMethodCRUD(c.rail), c.rail)
		require.Equal(t, c.autoBilledNil, AutoBilled(c.rail, nil), c.rail)
		require.Equal(t, c.autoBilledDelegated, AutoBilled(c.rail, delegated), c.rail)
		require.Equal(t, c.merchantKeys, MerchantCredentialKeyNames(c.rail), c.rail)
		require.Equal(t, c.activeCancel, CancelModeFor(&models.Subscription{Rail: c.rail, Status: models.StatusActive}, time.Now()), c.rail)
	}

	// Required keys gate arming; the Solana RPC endpoint is operator-only and
	// no PSP holds a signing key.
	k, ok := CredentialKeyFor(models.RailSolana, " RPC_URL ")
	require.True(t, ok)
	require.False(t, k.MerchantWritable)
	_, ok = CredentialKeyFor(models.RailSolana, "private_key")
	require.False(t, ok)
	for rail, environments := range map[models.Rail][]string{models.RailNMI: {"live", "test", "sandbox"}, models.RailStripe: {"live", "test"}, models.RailCCBill: {"live", "sandbox"}, models.RailSolana: {"mainnet", "devnet", "testnet"}} {
		require.Equal(t, environments, Environments(rail), rail)
		require.Equal(t, environments[0], DefaultEnvironment(rail), rail)
		for _, environment := range environments {
			require.Equal(t, environment == environments[0], LiveEnvironment(rail, environment), "%s %s", rail, environment)
		}
		require.Equal(t, AccountFields(rail), Fields(rail)[:len(AccountFields(rail))], rail)
	}
	// Each rail names its account in its own terms; the account they join to
	// is the identity ids and webhook paths have always used.
	for rail, row := range map[models.Rail]struct {
		fields  map[string]string
		account string
	}{
		models.RailNMI:    {map[string]string{"gateway_id": "1233466"}, "1233466"},
		models.RailStripe: {map[string]string{"account_id": "acct_1AbC"}, "acct_1AbC"},
		models.RailCCBill: {map[string]string{"client_accnum": "945280", "client_subacc": "0000"}, "945280-0000"},
		models.RailSolana: {map[string]string{"treasury": "CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w"}, "CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w"},
	} {
		require.Equal(t, row.account, Account(rail, row.fields), rail)
		require.Equal(t, row.fields, AccountParts(rail, row.account), rail)
	}
	require.Equal(t, "945280-", Account(models.RailCCBill, map[string]string{"client_accnum": "945280"}), "a missing part stays missing")
	require.Empty(t, Account(models.RailCCBill, nil))
	k, ok = CredentialKeyFor(models.RailStripe, "webhook_signing_secret_previous")
	require.True(t, ok)
	require.False(t, k.Required, "the rollover secret is optional")
	_, ok = CredentialKeyFor(models.RailNMI, "api_key")
	require.False(t, ok, "a custodian key is not an NMI credential (or#880)")
}

// NMI cancellation is reversible only while canceled, delete still pending, and paid period ahead.
func TestCancelModeFor(t *testing.T) {
	t.Parallel()
	now := time.Now()
	future, past := now.Add(72*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		sub  *models.Subscription
		want CancelMode
	}{
		{"nmi delete pending", &models.Subscription{Rail: models.RailNMI, Status: models.StatusCanceled, DeletionScheduledAt: &future, CurrentPeriodEndsAt: &future}, CancelModeReversible},
		{"nmi delete executed", &models.Subscription{Rail: models.RailNMI, Status: models.StatusCanceled, CurrentPeriodEndsAt: &future}, CancelModeDestructive},
		{"nmi period lapsed", &models.Subscription{Rail: models.RailNMI, Status: models.StatusCanceled, DeletionScheduledAt: &future, CurrentPeriodEndsAt: &past}, CancelModeDestructive},
		{"nmi period ends now", &models.Subscription{Rail: models.RailNMI, Status: models.StatusCanceled, DeletionScheduledAt: &future, CurrentPeriodEndsAt: &now}, CancelModeDestructive},
		{"engine owns its schedule", &models.Subscription{Rail: models.RailCCBill, CollectionPolicy: models.CollectionPolicyEngine}, CancelModeReversible},
		{"unknown rail", &models.Subscription{Rail: "bogus"}, CancelModeDestructive},
		{"nil", nil, CancelModeDestructive},
	} {
		require.Equal(t, tc.want, CancelModeFor(tc.sub, now), tc.name)
	}
}

func TestRailNormalization(t *testing.T) {
	t.Parallel()
	_, ok := Lookup(" STRIPE ")
	require.True(t, ok)
	_, ok = Lookup("mobius") // a PSP name on rail nmi, not a rail
	require.False(t, ok)
	require.False(t, HasRemoteCustomer("bogus") || SupportsPSPs("bogus") || AutoBilled("bogus", nil) || RemoteDeleteOnTerminalCancel("bogus") || SupportsCatalogTrial("bogus"))
	require.Nil(t, CredentialKeys("bogus"))
	require.Empty(t, CancelPortalURL("bogus"))
	require.Equal(t, "BOGUS", DisplayName(" bogus "))
	require.Equal(t, "", DisplayName(""))
	require.Equal(t, "Stripe", DisplayName(models.RailStripe))
	require.Equal(t, "Credit Card", DisplayName(" NMI "))

	require.True(t, IsNMI(" Nmi "))
	require.False(t, IsNMI("mobius"))
	require.True(t, SameRail(" Nmi ", models.RailNMI))
	require.False(t, SameRail(models.RailNMI, "other_nmi"))
	require.False(t, SameRail("", " "), "empty rails never match")
}
