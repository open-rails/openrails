package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/stretchr/testify/require"
)

const (
	usdcMint          = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	testPlanCreatedAt = int64(1_717_200_000)
)

// fakeChain is the Solana program boundary: it records create_plan submissions
// as plan accounts and serves SPL mint accounts with the given decimals.
type fakeChain struct {
	owner    solanago.PublicKey
	treasury solanago.PublicKey
	mints    map[string]uint8 // mint -> decimals
	accounts map[solanago.PublicKey][]byte
	submits  int
	readOnly bool
	failATA  bool
}

func newFakeChain(mints map[string]uint8) *fakeChain {
	return &fakeChain{owner: solanago.NewWallet().PublicKey(), treasury: solanago.NewWallet().PublicKey(), mints: mints, accounts: map[solanago.PublicKey][]byte{}}
}

func (c *fakeChain) Publisher(context.Context) (solanago.PublicKey, error) {
	return c.owner, nil
}

func (c *fakeChain) Signer(_ context.Context, owner solanago.PublicKey) (solanaint.Signer, error) {
	if !owner.Equals(c.owner) {
		return nil, fmt.Errorf("%w (owner %s)", recurring.ErrUnheldPlan, owner)
	}
	return nil, nil
}

// Treasury is the merchant's treasury.
func (c *fakeChain) Treasury(context.Context, billing.MerchantID) (solanago.PublicKey, error) {
	return c.treasury, nil
}

func (c *fakeChain) Submit(_ context.Context, _ billing.MerchantID, _ solanago.PublicKey, instructions []solanago.Instruction) (solanago.Signature, error) {
	c.submits++
	if c.readOnly {
		return solanago.Signature{}, fmt.Errorf("provider writes forbidden")
	}
	for _, ix := range instructions {
		if ix.ProgramID() == subscriptions.AssociatedTokenProgramID {
			if c.failATA {
				return solanago.Signature{}, fmt.Errorf("transaction dropped")
			}
			c.accounts[ix.Accounts()[1].PublicKey] = []byte{1}
			continue
		}
		if ix.ProgramID() != subscriptions.ProgramID {
			continue
		}
		data, err := ix.Data()
		if err != nil {
			return solanago.Signature{}, err
		}
		// Plan account: discriminator, owner, bump, status, then create_plan's args.
		blob := append(append([]byte{1}, c.owner.Bytes()...), 254, 0)
		blob = append(blob, data[1:]...)
		const createdAt = 1 + 32 + 1 + 1 + 8 + 32 + 8 + 8
		binary.LittleEndian.PutUint64(blob[createdAt:createdAt+8], uint64(testPlanCreatedAt))
		c.accounts[ix.Accounts()[1].PublicKey] = blob
	}
	return solanago.Signature{1}, nil
}

func (c *fakeChain) GetAccountData(_ context.Context, addr solanago.PublicKey) ([]byte, error) {
	if decimals, ok := c.mints[addr.String()]; ok {
		blob := make([]byte, solanaint.MintAccountSize)
		blob[44], blob[45] = decimals, 1
		return blob, nil
	}
	return c.accounts[addr], nil
}

func solanaFixture(chain *fakeChain, network string, tokens map[string]string, reader bool) *solanaAdapter {
	cfg := map[string]config.TokenConfig{}
	for symbol, mint := range tokens {
		cfg[symbol] = config.TokenConfig{Mint: mint}
	}
	plan := recurring.NewPlanServiceWithReader(chain, nil, recurring.NetworkTokens(network, cfg), chain.Treasury)
	if reader {
		plan = recurring.NewPlanServiceWithReader(chain, chain, recurring.NetworkTokens(network, cfg), chain.Treasury)
	}
	return &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
}

func solanaCtx() context.Context {
	return merchant.WithID(context.Background(), billing.MerchantID(uuid.New()))
}

func recurringTerms(micros int64) autoCreateContext {
	return autoCreateContext{PriceID: priceDeterministicID(uuid.MustParse("11111111-1111-4111-8111-111111111111"), "monthly", micros, "USD", intPtr(720), intPtr(720), nil, nil), ProductKey: "premium", Currency: "usd", UnitAmount: micros, BillingIntervalHours: intPtr(30 * 24), BillingCycleDays: intPtr(30)}
}

// Catalog micros become token BASE UNITS at the mint's on-chain decimals;
// micros verbatim undercharge 1000x on a 9-decimal mint.
func TestSolanaPlanPublishUsesOnChainMintDecimals(t *testing.T) {
	for _, tc := range []struct {
		decimals uint8
		micros   int64
		want     uint64
	}{
		{6, 10_000_000, 10_000_000}, {9, 10_000_000, 10_000_000_000}, {8, 10_000_000, 1_000_000_000},
		{6, 4_030_000, 4_030_000}, {9, 8_050_000, 8_050_000_000}, {8, 8_130_000, 813_000_000}, // float off-by-ones
		{9, 19_990_000, 19_990_000_000},
	} {
		chain := newFakeChain(map[string]uint8{usdcMint: tc.decimals})
		out, err := solanaFixture(chain, "mainnet", map[string]string{"USDC": usdcMint}, true).Attach(solanaCtx(), map[string]string{solanaKeyToken: "USDC"}, recurringTerms(tc.micros))
		require.NoError(t, err)
		require.Equal(t, strconv.FormatUint(tc.want, 10), out[solanaKeyAmountBaseUnits], "%d micros @%d", tc.micros, tc.decimals)
		require.Equal(t, strconv.FormatInt(testPlanCreatedAt, 10), out["created_at"], "created_at is read back from chain")
		require.Equal(t, "USDC", out[solanaKeyMintSymbol])
	}

	// No authoritative decimals (mint absent, or no chain reader): refuse, never guess.
	chain := newFakeChain(nil)
	_, err := solanaFixture(chain, "mainnet", map[string]string{"USDC": usdcMint}, true).Attach(solanaCtx(), map[string]string{solanaKeyToken: "USDC"}, recurringTerms(10_000_000))
	require.Error(t, err)
	_, err = solanaFixture(newFakeChain(map[string]uint8{usdcMint: 6}), "mainnet", map[string]string{"USDC": usdcMint}, false).AutoCreate(solanaCtx(), recurringTerms(10_000_000))
	require.Error(t, err)
	require.Zero(t, chain.submits)
}

func TestSolanaAdapterBranches(t *testing.T) {
	out, err := solanaFixture(newFakeChain(map[string]uint8{usdcMint: 6}), "mainnet", map[string]string{"USDC": usdcMint}, true).AutoCreate(solanaCtx(), recurringTerms(29_000_000))
	require.NoError(t, err)
	require.Equal(t, "USDC", out[solanaKeyMintSymbol], "mainnet recurring bills in USDC")

	devnet := solanaFixture(newFakeChain(map[string]uint8{solanatokens.DevnetUSDCMint: 6}), "devnet", map[string]string{"USDC": solanatokens.DevnetUSDCMint}, true)
	out, err = devnet.AutoCreate(solanaCtx(), recurringTerms(29_000_000))
	require.NoError(t, err)
	require.Equal(t, "USDC", out[solanaKeyMintSymbol], "devnet recurring bills in devnet USDC")
	require.Equal(t, solanatokens.DevnetUSDCMint, out[solanaKeyMint])

	out, err = (&solanaAdapter{}).AutoCreate(context.Background(), autoCreateContext{Currency: "eur", UnitAmount: 29_000_000, BillingIntervalHours: intPtr(720)})
	require.NoError(t, err)
	require.Equal(t, "solana", out["provider"], "one-off prices need no on-chain plan")

	writesOff := recurringTerms(23_000_000)
	writesOff.RemoteWritesDisabled = true
	chain := newFakeChain(map[string]uint8{usdcMint: 6})
	_, err = solanaFixture(chain, "mainnet", map[string]string{"USDC": usdcMint}, true).Attach(solanaCtx(), map[string]string{solanaKeyToken: "USDC"}, writesOff)
	require.ErrorIs(t, err, errRemoteWritesDisabled)
	require.Zero(t, chain.submits)

	cadence := autoCreateContext{BillingCycleDays: intPtr(30), Currency: "usd"}
	for name, link := range map[string]map[string]string{
		"mint_symbol is output only":         {solanaKeyMintSymbol: "USDC"},
		"non-recurring token":                {solanaKeyToken: "SOL"},
		"token beside plan_pda":              {solanaKeyPlanPDA: "PdA111", solanaKeyToken: "USDC"},
		"legacy plan_pda needs verification": {solanaKeyPlanPDA: "PdA111", solanaKeyAmountBaseUnits: "29000000"},
		"plan_pda without RPC":               {solanaKeyPlanPDA: "PdAWithoutToken111"},
	} {
		_, err := (&solanaAdapter{}).Attach(context.Background(), link, cadence)
		require.Error(t, err, name)
	}

	// An unreadable chain is sync_disabled, never an in-sync verdict.
	drift, missing, err := (&solanaAdapter{svc: &Service{}}).Verify(context.Background(), map[string]string{solanaKeyPlanPDA: "x"}, nil)
	require.ErrorIs(t, err, errProviderNotArmed)
	require.False(t, missing)
	require.Nil(t, drift)

	unconfigured := newFakeChain(nil)
	plan := recurring.NewPlanServiceWithReader(unconfigured, nil, recurring.NetworkTokens("mainnet", map[string]config.TokenConfig{"USDC": {Mint: usdcMint}}), unconfigured.Treasury)
	token, err := resolveSolanaTokenFromMint(t.Context(), plan, usdcMint)
	require.NoError(t, err)
	require.Equal(t, "USDC", token)
	_, err = resolveSolanaTokenFromMint(t.Context(), plan, solanago.NewWallet().PublicKey().String())
	require.Error(t, err, "unconfigured mint")
}

// A catalog apply whose plan landed but whose receiving ATA did not is healed
// by re-apply.
func TestSolanaCatalogReapplyEnsuresReceivingATA(t *testing.T) {
	chain := newFakeChain(map[string]uint8{usdcMint: 6})
	chain.failATA = true
	adapter := solanaFixture(chain, "mainnet", map[string]string{"USDC": usdcMint}, true)
	ctx := solanaCtx()
	ata, _, err := subscriptions.DeriveATA(chain.treasury, solanago.MustPublicKeyFromBase58(usdcMint), solanago.TokenProgramID)
	require.NoError(t, err)

	_, err = adapter.AutoCreate(ctx, recurringTerms(10_000_000))
	require.ErrorContains(t, err, "ensure receiving ata")
	require.Nil(t, chain.accounts[ata])

	chain.failATA = false
	out, err := adapter.AutoCreate(ctx, recurringTerms(10_000_000))
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(testPlanCreatedAt, 10), out["created_at"], "attached to the landed plan")
	require.NotNil(t, chain.accounts[ata], "re-apply created the receiving ATA")

	submits := chain.submits
	_, err = adapter.AutoCreate(ctx, recurringTerms(10_000_000))
	require.NoError(t, err)
	require.Equal(t, submits, chain.submits, "a healthy plan re-applies without writes")
}

// Reference preflight only reads: an existing plan must match owner, amount,
// period and status exactly, and a missing plan is never created here.
func TestSolanaCatalogReferencePreflight(t *testing.T) {
	req := billing.CreatePriceParams{Currency: "USD", UnitAmount: 23_000_000, AccessDurationHours: intPtr(72), BillingIntervalHours: intPtr(720)}
	setup := func() (*fakeChain, *recurring.PlanService, solanago.PublicKey) {
		chain := newFakeChain(map[string]uint8{usdcMint: 6})
		chain.readOnly = true
		mint := solanago.MustPublicKeyFromBase58(usdcMint)
		id := solanaPlanID(recurringTerms(23_000_000).PriceID, usdcMint, chain.treasury)
		address, bump, err := subscriptions.DerivePlanPDA(chain.owner, id)
		require.NoError(t, err)
		raw := make([]byte, subscriptions.PlanAccountSize)
		raw[0] = 1
		copy(raw[1:33], chain.owner[:])
		raw[33], raw[34] = bump, subscriptions.PlanStatusActive
		binary.LittleEndian.PutUint64(raw[35:43], id)
		copy(raw[43:75], mint[:])
		binary.LittleEndian.PutUint64(raw[75:83], 23_000_000)
		binary.LittleEndian.PutUint64(raw[83:91], 720)
		binary.LittleEndian.PutUint64(raw[91:99], 1_700_000_000)
		copy(raw[107:139], chain.treasury[:])
		chain.accounts[address] = raw
		return chain, recurring.NewPlanServiceWithReader(chain, chain, recurring.NetworkTokens("mainnet", map[string]config.TokenConfig{"USDC": {Mint: usdcMint}}), chain.Treasury), address
	}

	chain, plan, address := setup()
	_, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, req, map[string]string{solanaKeyPlanPDA: address.String()})
	require.NoError(t, err)

	planID := solanaPlanID(recurringTerms(req.UnitAmount).PriceID, usdcMint, chain.treasury)
	for _, link := range []map[string]string{
		{solanaKeyPlanID: strconv.FormatUint(planID, 10)},
		{solanaKeyPlanID: strconv.FormatUint(planID, 10), solanaKeyMint: usdcMint, solanaKeyMerchant: chain.owner.String()},
		{solanaKeyPlanID: "0" + strconv.FormatUint(planID, 10), solanaKeyToken: "USDC"},
	} {
		verified, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, req, link)
		require.NoError(t, err, link)
		require.Equal(t, address.String(), verified[solanaKeyPlanPDA])
		require.Equal(t, strconv.FormatUint(planID, 10), verified[solanaKeyPlanID])
	}
	for name, link := range map[string]map[string]string{
		"negative ID":         {solanaKeyPlanID: "-1"},
		"overflow ID":         {solanaKeyPlanID: "18446744073709551616"},
		"nondecimal ID":       {solanaKeyPlanID: "0x10"},
		"wrong ID beside PDA": {solanaKeyPlanID: strconv.FormatUint(planID+1, 10), solanaKeyPlanPDA: address.String()},
		"wrong mint":          {solanaKeyPlanID: strconv.FormatUint(planID, 10), solanaKeyMint: solanago.NewWallet().PublicKey().String()},
		"wrong owner":         {solanaKeyPlanID: strconv.FormatUint(planID, 10), solanaKeyMerchant: solanago.NewWallet().PublicKey().String()},
	} {
		_, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, req, link)
		require.Error(t, err, name)
	}
	require.Zero(t, chain.submits, "explicit reference verification never publishes a plan")

	delete(chain.accounts, address)
	_, err = verifySolanaCatalogReference(solanaCtx(), plan, chain, req, map[string]string{solanaKeyPlanID: strconv.FormatUint(planID, 10)})
	require.ErrorContains(t, err, "is missing")
	require.Zero(t, chain.submits)
	_, err = verifySolanaCatalogReference(solanaCtx(), plan, chain, req, nil)
	require.ErrorContains(t, err, "plan_pda or plan_id is required")

	for name, corrupt := range map[string]func([]byte){
		"foreign owner":    func(raw []byte) { foreign := solanago.NewWallet().PublicKey(); copy(raw[1:33], foreign[:]) },
		"another treasury": func(raw []byte) { other := solanago.NewWallet().PublicKey(); copy(raw[107:139], other[:]) },
		"amount":           func(raw []byte) { binary.LittleEndian.PutUint64(raw[75:83], 19_000_000) },
		"period":           func(raw []byte) { binary.LittleEndian.PutUint64(raw[83:91], 744) },
		"sunset":           func(raw []byte) { raw[34] = subscriptions.PlanStatusSunset },
	} {
		chain, plan, address := setup()
		corrupt(chain.accounts[address])
		_, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, req, map[string]string{solanaKeyPlanPDA: address.String()})
		require.Error(t, err, name)
		require.Zero(t, chain.submits, name)
	}
	require.Zero(t, chain.submits)
}

// unsignedChain is a deployment with no cranker to own plans.
type unsignedChain struct{ *fakeChain }

func (unsignedChain) Publisher(context.Context) (solanago.PublicKey, error) {
	return solanago.PublicKey{}, recurring.ErrNoCranker
}

// Without the deployment's cranker to own the plan, catalog application
// refuses the recurring price by name, before any submission; so does a
// runtime that armed no plan service.
func TestCatalogSolanaPlanNeedsTheCranker(t *testing.T) {
	chain := newFakeChain(map[string]uint8{usdcMint: 6})
	tokens := recurring.NetworkTokens("mainnet", map[string]config.TokenConfig{"USDC": {Mint: usdcMint}})
	svc := &Service{rt: &app.Runtime{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		SolanaPlanService: recurring.NewPlanServiceWithReader(unsignedChain{chain}, chain, tokens, chain.Treasury)}}
	terms := recurringTerms(23_000_000)
	check := catalogReferenceCheck{key: "monthly", productKey: "premium", priceID: terms.PriceID,
		request: billing.CreatePriceParams{Currency: "USD", UnitAmount: terms.UnitAmount, BillingIntervalHours: terms.BillingIntervalHours}}
	_, err := svc.findOrPublishCatalogSolanaPlan(solanaCtx(), check, nil)
	require.ErrorContains(t, err, `price "monthly" of product "premium": a recurring Solana price needs a listed solana.crankers`)
	require.ErrorContains(t, err, "solana rebilling is off")
	require.Zero(t, chain.submits)

	svc.rt.SolanaPlanService = nil
	_, err = svc.findOrPublishCatalogSolanaPlan(solanaCtx(), check, nil)
	require.ErrorContains(t, err, `price "monthly" of product "premium": solana rebilling is off: this deployment lists no solana.crankers`)
}
