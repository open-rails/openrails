package solana

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Canonical per-merchant Solana secret names. These mirror the merchant secret
// namespacing convention (e.g. "stripe/secret_key") so the SAME
// MerchantSecretStore that holds Stripe/NMI credentials also holds Solana ones.
// Only solana/private_key is sensitive; the addresses are public on-chain but
// are stored alongside for cohesion. A managed (Vault) deployment ideally keeps
// the private key in Vault Transit (non-extractable) instead — see RemoteSigner.
const (
	// SecretSolanaPrivateKey is the merchant's cranking signing keypair
	// (base58). It is the only sensitive Solana secret. Prefer Vault Transit in
	// production so this is never fetched in plaintext.
	SecretSolanaPrivateKey = "solana/private_key"
	// SecretSolanaMerchantAddress is the merchant's on-chain merchant address (the
	// plan owner; public). Derivable from the keypair, stored for convenience.
	SecretSolanaMerchantAddress = "solana/merchant_address"
	// SecretSolanaFeeWalletAddress is the merchant's gas (SOL) wallet address (public).
	SecretSolanaFeeWalletAddress = "solana/fee_wallet_address"
	// SecretSolanaRPCEndpoint is the merchant's RPC endpoint override (optional).
	SecretSolanaRPCEndpoint = "solana/rpc_endpoint"
	// SecretSolanaHeliusAPIKey is the merchant's Helius API key (optional).
	SecretSolanaHeliusAPIKey = "solana/helius_api_key"
)

// MerchantSecretGetter is the minimal per-merchant secret-read surface the signer
// needs. It is declared HERE (dependency inversion) rather than importing the
// merchants package, so the signing layer stays decoupled from the in-flight
// secret-store implementation (issue #227) and from any particular backend
// (DB+envelope or Vault). A thin adapter over merchants.MerchantSecretStore
// satisfies it at the composition root.
type MerchantSecretGetter interface {
	// GetSecret returns the plaintext secret value for (merchant, name), or an
	// error. Implementations MUST fail closed (never return "" + nil for a
	// missing secret) so a missing key can be distinguished from an empty one.
	GetSecret(ctx context.Context, merchantID merchant.ID, name string) (string, error)
}

// Signer produces Solana signatures for a merchant key WITHOUT exposing
// the private key to callers. It is resolved PER MERCHANT (via merchant.ID); there
// is no process-global signer. Two implementations exist:
//
//   - keypairSigner: loads solana/private_key from a MerchantSecretGetter and signs
//     in-process. Works with any secret backend; the key briefly lives in memory.
//   - (planned) RemoteSigner: Vault Transit — the key never leaves Vault.
//
// The interface is message-level (PublicKey + SignMessage) rather than
// Sign(tx) precisely so a remote signer can satisfy it: you can hand Vault the
// serialized message to sign, but you can never hand it the private key.
type Signer interface {
	// PublicKey returns the merchant address — the fee payer and sole
	// required signer on every plan/pull transaction this package builds.
	PublicKey(ctx context.Context, merchantID merchant.ID) (solanago.PublicKey, error)
	// SignMessage signs the raw serialized Solana message bytes
	// (Transaction.Message.MarshalBinary()) and returns the 64-byte signature.
	SignMessage(ctx context.Context, merchantID merchant.ID, message []byte) (solanago.Signature, error)
}

// blockhashProvider is the subset of *RPCClient the tx builder needs. Declared
// as an interface so BuildSignSubmit is unit-testable without a live RPC.
type blockhashProvider interface {
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
	SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error)
	SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, timeout time.Duration) (*TransactionOutcome, error)
}

// BuildSignSubmit assembles a single-signer transaction (the merchant is
// fee payer and sole required signer), signs its message via the per-merchant
// Signer, and submits it. This is the shared path for create_plan, update_plan,
// delete_plan, and transfer_subscription — only the instructions differ.
//
// With one required signer, Signatures[0] must correspond to the fee payer; if a
// future flow needs a co-signer, the signatures must be ordered to match the
// message's required-signer account list.
func BuildSignSubmit(
	ctx context.Context,
	merchantID merchant.ID,
	signer Signer,
	rpc blockhashProvider,
	instructions []solanago.Instruction,
) (solanago.Signature, error) {
	if signer == nil {
		return solanago.Signature{}, fmt.Errorf("solana: signer is required")
	}
	if rpc == nil {
		return solanago.Signature{}, fmt.Errorf("solana: rpc client is required")
	}
	if len(instructions) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: at least one instruction is required")
	}

	payer, err := signer.PublicKey(ctx, merchantID)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: resolve merchant signer public key: %w", err)
	}

	blockhash, err := rpc.GetLatestBlockhash(ctx)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: get recent blockhash: %w", err)
	}

	tx, err := solanago.NewTransaction(instructions, blockhash, solanago.TransactionPayer(payer))
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: build transaction: %w", err)
	}

	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: serialize transaction message: %w", err)
	}

	sig, err := signer.SignMessage(ctx, merchantID, msg)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: sign transaction: %w", err)
	}
	tx.Signatures = []solanago.Signature{sig}

	// Submit AND confirm: a billing pull must not be treated as success until the
	// transaction has actually landed. SubmitAndConfirm surfaces a reverted tx via
	// the outcome's on-chain error (with the program's Custom code) so the cranker
	// can classify it (#270) instead of silently "succeeding" on a failed pull.
	outcome, err := rpc.SubmitAndConfirm(ctx, tx, 0)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: submit/confirm transaction: %w", err)
	}
	if oerr := outcome.OnChainError(); oerr != nil {
		return solanago.Signature{}, oerr
	}
	return outcome.Signature, nil
}
