package solana

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Canonical per-tenant Solana secret names. These mirror tenancy's processor
// namespacing convention (e.g. "stripe/secret_key") so the SAME
// TenantSecretStore that holds Stripe/NMI credentials also holds Solana ones.
// Only solana/private_key is sensitive; the addresses are public on-chain but
// are stored alongside for cohesion. A managed (Vault) deployment ideally keeps
// the private key in Vault Transit (non-extractable) instead — see RemoteSigner.
const (
	// SecretSolanaPrivateKey is the tenant's merchant/cranking signing keypair
	// (base58). It is the only sensitive Solana secret. Prefer Vault Transit in
	// production so this is never fetched in plaintext.
	SecretSolanaPrivateKey = "solana/private_key"
	// SecretSolanaMerchantAddress is the tenant's on-chain merchant address (the
	// plan owner; public). Derivable from the keypair, stored for convenience.
	SecretSolanaMerchantAddress = "solana/merchant_address"
	// SecretSolanaFeeWalletAddress is the tenant's gas (SOL) wallet address (public).
	SecretSolanaFeeWalletAddress = "solana/fee_wallet_address"
	// SecretSolanaRPCEndpoint is the tenant's RPC endpoint override (optional).
	SecretSolanaRPCEndpoint = "solana/rpc_endpoint"
	// SecretSolanaHeliusAPIKey is the tenant's Helius API key (optional).
	SecretSolanaHeliusAPIKey = "solana/helius_api_key"
)

// TenantSecretGetter is the minimal per-tenant secret-read surface the signer
// needs. It is declared HERE (dependency inversion) rather than importing the
// tenancy package, so the signing layer stays decoupled from the in-flight
// secret-store implementation (issue #227) and from any particular backend
// (DB+envelope or Vault). A thin adapter over tenancy.TenantSecretStore
// satisfies it at the composition root.
type TenantSecretGetter interface {
	// GetSecret returns the plaintext secret value for (tenant, name), or an
	// error. Implementations MUST fail closed (never return "" + nil for a
	// missing secret) so a missing key can be distinguished from an empty one.
	GetSecret(ctx context.Context, tenantID tenant.ID, name string) (string, error)
}

// Signer produces Solana signatures for a tenant's merchant key WITHOUT exposing
// the private key to callers. It is resolved PER TENANT (via tenant.ID); there
// is no process-global signer. Two implementations exist:
//
//   - keypairSigner: loads solana/private_key from a TenantSecretGetter and signs
//     in-process. Works with any secret backend; the key briefly lives in memory.
//   - (planned) RemoteSigner: Vault Transit — the key never leaves Vault.
//
// The interface is message-level (PublicKey + SignMessage) rather than
// Sign(tx) precisely so a remote signer can satisfy it: you can hand Vault the
// serialized message to sign, but you can never hand it the private key.
type Signer interface {
	// PublicKey returns the tenant's merchant address — the fee payer and sole
	// required signer on every plan/pull transaction this package builds.
	PublicKey(ctx context.Context, tenantID tenant.ID) (solanago.PublicKey, error)
	// SignMessage signs the raw serialized Solana message bytes
	// (Transaction.Message.MarshalBinary()) and returns the 64-byte signature.
	SignMessage(ctx context.Context, tenantID tenant.ID, message []byte) (solanago.Signature, error)
}

// blockhashProvider is the subset of *RPCClient the tx builder needs. Declared
// as an interface so BuildSignSubmit is unit-testable without a live RPC.
type blockhashProvider interface {
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
	SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error)
	SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, timeout time.Duration) (*TransactionOutcome, error)
}

// BuildSignSubmit assembles a single-signer transaction (the tenant merchant is
// fee payer and sole required signer), signs its message via the per-tenant
// Signer, and submits it. This is the shared path for create_plan, update_plan,
// delete_plan, and transfer_subscription — only the instructions differ.
//
// With one required signer, Signatures[0] must correspond to the fee payer; if a
// future flow needs a co-signer, the signatures must be ordered to match the
// message's required-signer account list.
func BuildSignSubmit(
	ctx context.Context,
	tenantID tenant.ID,
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

	payer, err := signer.PublicKey(ctx, tenantID)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: resolve tenant signer public key: %w", err)
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

	sig, err := signer.SignMessage(ctx, tenantID, msg)
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
