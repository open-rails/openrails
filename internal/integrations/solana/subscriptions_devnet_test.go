//go:build devnet

// Live devnet integration tests for the Solana Subscriptions program. These are
// NOT unit tests with mocks: each generates a fresh keypair, airdrops real
// devnet SOL from the faucet, and submits real on-chain transactions against the
// deployed program (De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44).
//
// Run explicitly (they are slow and need network):
//
//	go test -tags devnet -run Devnet -timeout 600s ./internal/integrations/solana/...
//
// They exercise the FULL real stack: keypairSigner -> BuildSignSubmit ->
// subscriptions instruction builders -> devnet RPC. A green run validates the
// DEVNET-VERIFY items (PDA derivation, account orderings, arg encoding) against
// the real program.
package solana

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

const (
	devnetRPC = "https://api.devnet.solana.com"
	// Hardcoded devnet mints (also in config/solana_tokens.go).
	devnetUSDCMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	devnetPYUSDMint = "CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM"
)

// staticSecret is a TenantSecretGetter that returns one fixed key, so the test
// drives the REAL keypairSigner + BuildSignSubmit path (not a bespoke signer).
type staticSecret struct{ base58 string }

func (s staticSecret) GetSecret(context.Context, tenant.ID, string) (string, error) {
	return s.base58, nil
}

// devnetPayerEnv names the env var holding a base58 devnet private key that is
// ALREADY funded (via https://faucet.solana.com). When set, tests use it as the
// merchant/payer and skip the (rate-limited) RPC airdrop entirely. Fund one key
// once and these tests run reliably:
//
//	export SOLANA_DEVNET_PAYER_KEY=<base58 devnet private key>
const devnetPayerEnv = "SOLANA_DEVNET_PAYER_KEY"

// fundedMerchant returns a usable, funded devnet keypair: the env payer if set,
// otherwise a fresh keypair funded by RPC airdrop (which skips the test if the
// faucet is rate-limited/dry). Different runs use a unique plan_id, so reusing
// one persistent payer across runs is safe.
func fundedMerchant(ctx context.Context, t *testing.T, faucet *rpc.Client) solanago.PrivateKey {
	t.Helper()
	if raw := strings.TrimSpace(os.Getenv(devnetPayerEnv)); raw != "" {
		key, err := solanago.PrivateKeyFromBase58(raw)
		if err != nil {
			t.Fatalf("%s is not a valid base58 private key: %v", devnetPayerEnv, err)
		}
		bal, err := faucet.GetBalance(ctx, key.PublicKey(), rpc.CommitmentConfirmed)
		if err == nil && bal.Value < 5_000_000 { // ~0.005 SOL: below create_plan rent + fee
			t.Skipf("%s (%s) balance is %d lamports — fund it at https://faucet.solana.com", devnetPayerEnv, key.PublicKey(), bal.Value)
		}
		t.Logf("using funded payer %s", key.PublicKey())
		return key
	}
	merchant, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	airdrop(ctx, t, faucet, merchant.PublicKey(), 1)
	return merchant
}

// airdrop funds addr with `sol` SOL on devnet and waits for the balance to land.
func airdrop(ctx context.Context, t *testing.T, cl *rpc.Client, addr solanago.PublicKey, sol uint64) {
	t.Helper()
	lamports := sol * 1_000_000_000
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := cl.RequestAirdrop(ctx, addr, lamports, rpc.CommitmentConfirmed); err != nil {
			lastErr = err
			time.Sleep(3 * time.Second)
			continue
		}
		// Poll for the balance to appear.
		for i := 0; i < 30; i++ {
			bal, err := cl.GetBalance(ctx, addr, rpc.CommitmentConfirmed)
			if err == nil && bal.Value > 0 {
				t.Logf("airdrop landed: %d lamports", bal.Value)
				return
			}
			time.Sleep(2 * time.Second)
		}
		lastErr = nil
	}
	if lastErr != nil {
		t.Skipf("devnet faucet unavailable (airdrop failed): %v", lastErr)
	}
	t.Skip("devnet airdrop did not land in time; faucet likely rate-limited")
}

// buildCreatePlanFor builds a create_plan tx for the given mint/token program and
// submits it via the real signer + tx builder, returning any submit error.
func submitCreatePlan(ctx context.Context, t *testing.T, merchant solanago.PrivateKey, mint, tokenProgram solanago.PublicKey) error {
	t.Helper()
	signer := NewKeypairSigner(staticSecret{base58: merchant.String()}, time.Minute)
	rpcClient := NewRPCClientWithConfig(RPCClientConfig{Endpoint: devnetRPC, Network: "devnet"})

	planID := uint64(time.Now().UnixNano())
	planPDA, _, err := subscriptions.DerivePlanPDA(merchant.PublicKey(), planID)
	if err != nil {
		t.Fatalf("derive plan pda: %v", err)
	}

	ix, err := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
		Merchant:     merchant.PublicKey(),
		PlanPDA:      planPDA,
		Mint:         mint,
		TokenProgram: tokenProgram,
		PlanID:       planID,
		Terms: subscriptions.PlanTerms{
			Amount:      10_000_000, // 10 USDC (6 decimals)
			PeriodHours: 720,        // 30 days
			CreatedAt:   time.Now().Unix(),
		},
		EndTs:       0,
		MetadataURI: "",
	})
	if err != nil {
		t.Fatalf("build create_plan: %v", err)
	}

	sig, err := BuildSignSubmit(ctx, tenant.DefaultID, signer, rpcClient, []solanago.Instruction{ix})
	if err != nil {
		return err
	}
	t.Logf("submitted create_plan, sig=%s", sig)
	return rpcClient.ConfirmTransaction(ctx, sig, rpc.CommitmentConfirmed)
}

// TestDevnetCreatePlanUSDC verifies the create_plan encoder + PDA derivation +
// account ordering are correct: a USDC plan must be ACCEPTED on devnet.
func TestDevnetCreatePlanUSDC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	faucet := rpc.New(devnetRPC)
	merchant := fundedMerchant(ctx, t, faucet)

	if err := submitCreatePlan(ctx, t, merchant, solanago.MustPublicKeyFromBase58(devnetUSDCMint), solanago.TokenProgramID); err != nil {
		t.Fatalf("create_plan(USDC) was rejected on devnet — encoder/account layout likely wrong: %v", err)
	}
	t.Log("create_plan(USDC) ACCEPTED on devnet — encoder validated")
}

// TestDevnetCreatePlanPYUSDRejected answers issue #252: PYUSD's Token-2022 mint
// (PermanentDelegate + TransferFee extensions) should be REJECTED by create_plan.
// If it is unexpectedly accepted, PYUSD could be added to the recurring allowlist.
func TestDevnetCreatePlanPYUSDRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	faucet := rpc.New(devnetRPC)
	merchant := fundedMerchant(ctx, t, faucet)

	err := submitCreatePlan(ctx, t, merchant, solanago.MustPublicKeyFromBase58(devnetPYUSDMint), solanago.Token2022ProgramID)
	if err == nil {
		t.Log("#252 RESULT: create_plan(PYUSD) was ACCEPTED — PYUSD MAY be recurring-eligible; re-evaluate allowlist")
		return
	}
	t.Logf("#252 RESULT: create_plan(PYUSD) REJECTED as expected (%v) — keep recurring USDC-only", strings.TrimSpace(err.Error()))
}
