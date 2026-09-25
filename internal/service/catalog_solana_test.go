package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
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
	mints    map[string]uint8 // mint -> decimals
	accounts map[solanago.PublicKey][]byte
	submits  int
	readOnly bool
	failATA  bool
}

func newFakeChain(mints map[string]uint8) *fakeChain {
	return &fakeChain{owner: solanago.NewWallet().PublicKey(), mints: mints, accounts: map[solanago.PublicKey][]byte{}}
}

func (c *fakeChain) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return c.owner, nil
}

func (c *fakeChain) Submit(_ context.Context, _ merchant.ID, instructions []solanago.Instruction) (solanago.Signature, error) {
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
	plan := recurring.NewPlanServiceWithReader(chain, nil, network, cfg)
	if reader {
		plan = recurring.NewPlanServiceWithReader(chain, chain, network, cfg)
	}
	return &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
}

func solanaCtx() context.Context {
	return merchant.WithID(context.Background(), merchant.ID(uuid.New()))
}

func recurringTerms(micros int64) autoCreateContext {
	return autoCreateContext{PriceID: uuid.New(), ProductKey: "premium", Currency: "usd", UnitAmount: micros, AccessDurationHours: intPtr(30 * 24), BillingCycleDays: intPtr(30)}
}

// #817: catalog micros become token BASE UNITS using the mint's on-chain
// decimals; shipping micros verbatim undercharged 1000x on a 9-decimal mint.
func TestSolanaPlanPublishUsesOnChainMintDecimals(t *testing.T) {
	for _, tc := range []struct {
		decimals uint8
		micros   int64
		want     uint64
	}{
		{6, 10_000_000, 10_000_000}, {9, 10_000_000, 10_000_000_000}, {8, 10_000_000, 1_000_000_000},
		{6, 4_030_000, 4_030_000}, {9, 8_050_000, 8_050_000_000}, {8, 8_130_000, 813_000_000}, // #818 float off-by-ones
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
	const dusdMint = "7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T"
	out, err := solanaFixture(newFakeChain(map[string]uint8{usdcMint: 6}), "mainnet", map[string]string{"USDC": usdcMint}, true).AutoCreate(solanaCtx(), recurringTerms(29_000_000))
	require.NoError(t, err)
	require.Equal(t, "USDC", out[solanaKeyMintSymbol], "live recurring defaults to USDC")

	sandbox := solanaFixture(newFakeChain(map[string]uint8{dusdMint: 6}), "devnet", map[string]string{"DUSD": dusdMint}, true)
	sandbox.svc.rt.Config = &config.Config{TestMode: config.CredentialPostureSandbox}
	out, err = sandbox.AutoCreate(solanaCtx(), recurringTerms(29_000_000))
	require.NoError(t, err)
	require.Equal(t, "DUSD", out[solanaKeyMintSymbol], "sandbox recurring defaults to DUSD")

	out, err = (&solanaAdapter{}).AutoCreate(context.Background(), autoCreateContext{Currency: "eur", UnitAmount: 29_000_000, AccessDurationHours: intPtr(720)})
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

	plan := recurring.NewPlanServiceWithReader(newFakeChain(nil), nil, "mainnet", map[string]config.TokenConfig{"USDC": {Mint: usdcMint}, "USD1": {Mint: "USD1USD1USD1USD1USD1USD1USD1USD1USD1USD1USD"}})
	token, err := resolveSolanaTokenFromMint(plan, "USD1USD1USD1USD1USD1USD1USD1USD1USD1USD1USD")
	require.NoError(t, err)
	require.Equal(t, "USD1", token)
	_, err = resolveSolanaTokenFromMint(plan, solanago.NewWallet().PublicKey().String())
	require.Error(t, err, "unconfigured mint")
	_, err = resolveSolanaTokenFromMint(plan, "usd1usd1usd1usd1usd1usd1usd1usd1usd1usd1usd")
	require.Error(t, err, "base58 is case-sensitive")
}

// A catalog apply whose plan landed but whose receiving ATA did not is healed
// by re-apply.
func TestSolanaCatalogReapplyEnsuresReceivingATA(t *testing.T) {
	chain := newFakeChain(map[string]uint8{usdcMint: 6})
	chain.failATA = true
	adapter := solanaFixture(chain, "mainnet", map[string]string{"USDC": usdcMint}, true)
	ctx := solanaCtx()
	ata, _, err := subscriptions.DeriveATA(chain.owner, solanago.MustPublicKeyFromBase58(usdcMint), solanago.TokenProgramID)
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
	req := CreatePriceRequest{Currency: "USD", UnitAmount: 23_000_000, AccessDurationHours: intPtr(720), AutoRenew: true}
	setup := func() (*fakeChain, *recurring.PlanService, solanago.PublicKey) {
		chain := newFakeChain(map[string]uint8{usdcMint: 6})
		chain.readOnly = true
		mint := solanago.MustPublicKeyFromBase58(usdcMint)
		id := solanaPlanID("premium", "usd", 23_000_000, intPtr(720), usdcMint)
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
		chain.accounts[address] = raw
		return chain, recurring.NewPlanServiceWithReader(chain, chain, "mainnet", map[string]config.TokenConfig{"USDC": {Mint: usdcMint}}), address
	}

	chain, plan, address := setup()
	_, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, "USDC", "premium", req, map[string]string{solanaKeyPlanPDA: address.String()})
	require.NoError(t, err)

	delete(chain.accounts, address)
	_, err = verifySolanaCatalogReference(solanaCtx(), plan, chain, "USDC", "premium", req, nil)
	require.ErrorContains(t, err, "separate provider workflow")

	for name, corrupt := range map[string]func([]byte){
		"foreign owner": func(raw []byte) { foreign := solanago.NewWallet().PublicKey(); copy(raw[1:33], foreign[:]) },
		"amount":        func(raw []byte) { binary.LittleEndian.PutUint64(raw[75:83], 19_000_000) },
		"period":        func(raw []byte) { binary.LittleEndian.PutUint64(raw[83:91], 744) },
		"sunset":        func(raw []byte) { raw[34] = subscriptions.PlanStatusSunset },
	} {
		chain, plan, address := setup()
		corrupt(chain.accounts[address])
		_, err := verifySolanaCatalogReference(solanaCtx(), plan, chain, "USDC", "premium", req, map[string]string{solanaKeyPlanPDA: address.String()})
		require.Error(t, err, name)
		require.Zero(t, chain.submits, name)
	}
	require.Zero(t, chain.submits)
}
