//go:build devnet

// Devnet root-cause probe for the repeated-subscribe Custom:519 (#274). A tier
// change re-subscribes the SAME wallet (same SubscriptionAuthority) to a new
// plan, so we must understand what a 2nd subscribe needs. This subscribes one
// fresh wallet to plan A then plan B, dumping the SubscriptionAuthority account
// bytes before/after each to see what the program tracks (a counter?) and whether
// the 2nd subscribe fails. Needs only SOL (no USDC pull).
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
//	  go test -tags devnet -run SubscribeRepeat -v -timeout 300s ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestDevnetSubscribeRepeat519(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")

	subscriber, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fresh subscriber: %s", subscriber.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, subscriber.PublicKey(), 40_000_000)
	ensureATA(ctx, t, rc, raw, merchant, subscriber.PublicKey(), usdc) // for init's delegate

	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, rc, "devnet")

	saPDA, _, _ := subscriptions.DeriveSubscriptionAuthority(subscriber.PublicKey(), usdc)
	dumpSA := func(label string) {
		d, _ := rc.GetAccountData(ctx, saPDA)
		id, ok, _ := readAuthorityInitID(ctx, rc, saPDA)
		t.Logf("SA[%s] len=%d initId@98=%d ok=%v bytes=%s", label, len(d), id, ok, hex.EncodeToString(d))
	}

	publishAndSubscribe := func(label string) (string, bool) {
		h, err := planSvc.PublishPlan(ctx, PublishPlanInput{
			TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
			TokenSymbol: "USDC", AmountBaseUnits: 1_000_000, PeriodHours: 720,
		})
		if err != nil {
			t.Fatalf("publish %s: %v", label, err)
		}
		var lastErr string
		var ok bool
		for step := 0; step < 2; step++ {
			res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
				TenantID: tenant.DefaultID, SubscriberWallet: subscriber.PublicKey().String(),
				PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: 1_000_000, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
			})
			if perr != nil {
				t.Fatalf("prepare %s: %v", label, perr)
			}
			t.Logf("subscribe %s: step=%s", label, res.Step)
			lastErr, ok = trySignSend(ctx, t, raw, rc, subscriber, res.Transactions)
			if !ok {
				return lastErr, false
			}
			if res.Step == "subscribe" {
				break
			}
		}
		return "", true
	}

	dumpSA("before-any")
	if e, ok := publishAndSubscribe("A"); !ok {
		t.Fatalf("first subscribe (A) unexpectedly failed: %s", e)
	}
	dumpSA("after-A")
	e, ok := publishAndSubscribe("B")
	dumpSA("after-B")
	if ok {
		t.Log("FINDING: 2nd subscribe (B) by the SAME wallet SUCCEEDED — repeat-subscribe is fine; Custom:519 was something else (likely the read race, now fixed)")
	} else {
		t.Logf("FINDING: 2nd subscribe (B) FAILED on-chain = %s — compare the SA dumps above to find what the 2nd subscribe must echo (counter vs init_id)", e)
	}
}

// trySignSend deserializes + subscriber-signs + submits each base64 tx
// (skip-preflight + confirm), returning (onchainErr, ok). ok=false means a tx
// reverted; the error string is returned for inspection.
func trySignSend(ctx context.Context, t *testing.T, raw *rpc.Client, rc *solanaint.RPCClient, wallet solanago.PrivateKey, txns []string) (string, bool) {
	t.Helper()
	for _, b64 := range txns {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := solanago.TransactionFromBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		bh, err := rc.GetLatestBlockhash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tx.Message.RecentBlockhash = bh
		tx.Signatures = nil
		if _, err := tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
			if wallet.PublicKey().Equals(pk) {
				return &wallet
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sig, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
		if err != nil {
			return err.Error(), false
		}
		out, werr := rc.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, 90*time.Second)
		if werr != nil {
			return werr.Error(), false
		}
		if e := out.OnChainError(); e != nil {
			return e.Error(), false
		}
	}
	return "", true
}
