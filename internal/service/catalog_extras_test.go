package service

import (
	"context"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/stretchr/testify/require"
)

type extraClass struct {
	owned, active bool
	typ           string
}

func classify(extras []CatalogExtra) map[string]extraClass {
	out := map[string]extraClass{}
	for _, e := range extras {
		out[e.ExternalID] = extraClass{e.Owned, e.Active, e.ObjectType}
	}
	return out
}

// Objects the local catalog links by id or matches by content key are not
// extras; the rest are OWNED only when they carry an OpenRails marker.
func TestCatalogExtrasClassifyOwnership(t *testing.T) {
	productID := uuid.New()
	snap := catalog.BuildDriftSnapshot([]*models.Product{{ID: productID, Key: "premium"}}, []*models.Price{{
		ID: uuid.New(), ProductID: productID, Amount: 23_000_000, Currency: "USD", AccessDurationHours: intPtr(30 * 24), AutoRenew: true,
		PSPLinks: map[string]map[string]string{
			"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_local", models.RailKeyStripeProductID: "prod_local"},
			"mobius": {models.RailKeyRail: string(models.RailNMI), models.RailKeyPlanID: "premium-usd-23000000-30"},
		},
	}}, uuid.Nil)

	stripe := computeStripeExtras(
		[]catalog.StripeProduct{
			{ID: "prod_matched", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "premium"}},
			{ID: "prod_local", Active: true},
			{ID: "prod_ours_extra", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "retired"}},
			{ID: "prod_foreign", Active: true},
		},
		[]catalog.StripePrice{
			{ID: "price_matched", Active: true, LookupKey: "openrails.premium.usd.23000000.30"},
			{ID: "price_local", Active: true},
			{ID: "price_ours_extra", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsPriceKey: "retired.usd.9000000.30"}},
			{ID: "price_ours_inactive", LookupKey: "openrails.retired.usd.5000000.30"},
			{ID: "price_foreign", Active: true, Nickname: "merchant price"},
		}, snap)
	for _, e := range stripe {
		require.Equal(t, "stripe", e.Provider)
	}
	require.Equal(t, map[string]extraClass{
		"prod_ours_extra":     {true, true, "product"},
		"prod_foreign":        {false, true, "product"},
		"price_ours_extra":    {true, true, "price"},
		"price_ours_inactive": {true, false, "price"},
		"price_foreign":       {false, true, "price"},
	}, classify(stripe))

	nmi := computeNMIExtras([]catalog.NMIPlan{
		{PlanID: "premium-usd-23000000-30"}, {PlanID: "retired-usd-900-30"}, {PlanID: "legacy-vip-plan"},
	}, snap)
	require.Equal(t, map[string]extraClass{
		"retired-usd-900-30": {true, true, "plan"},
		"legacy-vip-plan":    {false, true, "plan"},
	}, classify(nmi))
}

type fakeIntentExecutor struct {
	calls  []intents.EnqueueParams
	status string
	reason string
}

func (f *fakeIntentExecutor) EnqueueAndExecute(_ context.Context, p intents.EnqueueParams) (gen.OpenrailsRailIntent, error) {
	f.calls = append(f.calls, p)
	row := gen.OpenrailsRailIntent{ID: uuid.New(), IntentType: p.IntentType, Rail: p.Provider, Status: f.status, ResultEvidence: []byte(`{"archived":true}`)}
	if f.reason != "" {
		row.LastFailureReason = &f.reason
	}
	return row, nil
}

// --prune archives only OWNED ACTIVE Stripe objects and Solana plans through
// admin-origin intents; foreign objects are never touched and NMI stays
// manual (its plan delete is unsafe).
func TestArchiveCatalogExtrasOnlyOwnedThroughIntents(t *testing.T) {
	const pda = "5tzFkiKscXHK5ZXCGbXZxdw7gTfCvqSGpHGxVJD6oxBd"
	merchantID := uuid.New()
	exec := &fakeIntentExecutor{status: intents.StatusSucceeded}
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours", Owned: true, Active: true, MarkerKey: "retired.usd.9000000.30"},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_ours", Owned: true, Active: true, MarkerKey: "retired"},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_foreign", Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours_inactive", Owned: true},
		{Provider: "nmi", ObjectType: "plan", ExternalID: "retired-usd-900-30", Owned: true, Active: true},
		{Provider: "nmi", ObjectType: "plan", ExternalID: "legacy-vip-plan", Active: true},
		{Provider: "solana", ObjectType: "plan", ExternalID: pda, Owned: true, Active: true},
	}
	outcomes, err := archiveCatalogExtrasVia(t.Context(), exec, merchantID, time.Now().UTC(), extras)
	require.NoError(t, err)
	actions := map[string]CatalogExtraArchiveAction{}
	for _, o := range outcomes {
		actions[o.Extra.ExternalID] = o.Action
	}
	require.Equal(t, map[string]CatalogExtraArchiveAction{
		"price_ours": CatalogExtraArchived, "prod_ours": CatalogExtraArchived, pda: CatalogExtraArchived,
		"price_foreign": CatalogExtraSkippedForeign, "legacy-vip-plan": CatalogExtraSkippedForeign,
		"price_ours_inactive": CatalogExtraSkippedInactive, "retired-usd-900-30": CatalogExtraManualActionRequired,
	}, actions)

	byType := map[string]intents.EnqueueParams{}
	for _, c := range exec.calls {
		require.Equal(t, intents.OriginAdmin, c.Origin, "--prune is a human request")
		require.Equal(t, merchantID, c.MerchantID)
		require.NotEqual(t, "nmi", c.Provider, "NMI has no archive write path")
		byType[c.IntentType] = c
	}
	require.Len(t, exec.calls, 3)
	require.Equal(t, intents.StripeArchiveIdempotencyKey(intents.TypeStripeArchivePrice, "price_ours"), byType[intents.TypeStripeArchivePrice].IdempotencyKey)
	require.Equal(t, intents.StripeArchivePayload{ObjectID: "prod_ours", MarkerKey: "retired"}, byType[intents.TypeStripeArchiveProduct].Payload)
	require.Equal(t, pda, byType[intents.TypeSolanaSunsetPlan].Payload.(intents.SolanaSunsetPayload).PlanPDA)
	for _, o := range outcomes {
		if o.Extra.Provider == "nmi" && o.Extra.Owned {
			require.Contains(t, o.Detail, "zero subscribers")
		}
	}
}

// #358-D: a parked intent (mode gate, provider down) is durable, not an
// error; terminal failures continue the pass and aggregate into an error.
func TestArchiveCatalogExtrasParkedVersusFailed(t *testing.T) {
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_a", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_b", Owned: true, Active: true},
	}
	parked, err := archiveCatalogExtrasVia(t.Context(), &fakeIntentExecutor{status: intents.StatusPending, reason: "mode=readonly blocks all provider writes"}, uuid.New(), time.Now(), extras)
	require.NoError(t, err)
	for _, o := range parked {
		require.Equal(t, CatalogExtraArchiveParked, o.Action)
		require.Contains(t, o.Detail, "mode=readonly")
		require.NotEmpty(t, o.IntentID, "parked outcomes reference the durable intent")
	}
	failed, err := archiveCatalogExtrasVia(t.Context(), &fakeIntentExecutor{status: intents.StatusFailedTerminal, reason: "stripe refused"}, uuid.New(), time.Now(), extras)
	require.Error(t, err)
	require.Len(t, failed, 2)
	for _, o := range failed {
		require.Equal(t, CatalogExtraArchiveFailed, o.Action)
	}
}

type solanaAccounts map[string][]byte

func (a solanaAccounts) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	return a[address.String()], nil
}

func planAccount(status uint8) []byte {
	blob := make([]byte, subscriptions.PlanAccountSize)
	blob[0], blob[33], blob[34] = 1, 1, status
	return blob
}

// Only an ARCHIVED price whose on-chain plan is still active needs a sunset;
// live prices are never read, sunset or absent plans have converged.
func TestComputeSolanaSunsetExtras(t *testing.T) {
	productID := uuid.New()
	price := func(pda string, archived bool) *models.Price {
		return &models.Price{ID: uuid.New(), ProductID: productID, Amount: 23_000_000, Currency: "USD", AccessDurationHours: intPtr(30 * 24), AutoRenew: true, Archived: archived,
			PSPLinks: map[string]map[string]string{"solana": {models.RailKeyRail: "solana", "plan_pda": pda}}}
	}
	pda := func() string { return solanago.NewWallet().PublicKey().String() }
	activeArchived, sunsetArchived, absentArchived, activeLive := pda(), pda(), pda(), pda()
	snap := catalog.BuildDriftSnapshot([]*models.Product{{ID: productID, Key: "premium"}}, []*models.Price{
		price(activeArchived, true), price(sunsetArchived, true), price(absentArchived, true), price(activeLive, false),
	}, uuid.Nil)
	reader := solanaAccounts{activeArchived: planAccount(1), sunsetArchived: planAccount(0), activeLive: planAccount(1)}

	extras, scanned, notes := computeSolanaSunsetExtras(t.Context(), reader, snap)
	require.Empty(t, notes)
	require.Equal(t, 3, scanned)
	require.Equal(t, []CatalogExtra{{Provider: "solana", ObjectType: "plan", ExternalID: activeArchived, Label: "premium.usd.23000000.30", Owned: true, Active: true}}, extras)
}
