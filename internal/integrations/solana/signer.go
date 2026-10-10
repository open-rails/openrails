package solana

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
)

const privateKeySecretName = "private_key"

// MerchantSecretGetter is the per-merchant secret read the signer needs,
// declared here so signing is independent of the secret backend. An adapter
// over merchants.MerchantSecretReader satisfies it at the composition root.
type MerchantSecretGetter interface {
	// GetSecret returns the plaintext secret value for (merchant, name), or an
	// error. Implementations MUST fail closed (never return "" + nil for a
	// missing secret) so a missing key can be distinguished from an empty one.
	GetSecret(ctx context.Context, merchantID billing.MerchantID, name string) (string, error)
}

// Signer produces Solana signatures for a merchant key without exposing the
// private key. It is resolved per merchant; there is no process-global signer.
// keypairSigner loads the PSP private_key secret and signs in-process;
// transitSigner signs through Vault Transit, so the key never leaves Vault.
// The interface is message-level so a remote signer can satisfy it.
type Signer interface {
	// PublicKey returns the merchant address — the fee payer and sole
	// required signer on every plan/pull transaction this package builds.
	PublicKey(ctx context.Context, merchantID billing.MerchantID) (solanago.PublicKey, error)
	// SignMessage signs the raw serialized Solana message bytes
	// (Transaction.Message.MarshalBinary()) and returns the 64-byte signature.
	SignMessage(ctx context.Context, merchantID billing.MerchantID, message []byte) (solanago.Signature, error)
}

// blockhashProvider is the subset of *RPCClient the tx builder needs. Declared
// as an interface so BuildSignSubmit is unit-testable without a live RPC.
type blockhashProvider interface {
	// LatestBlockhash returns the blockhash with its chain terminal, so the
	// confirmation watch ends on the chain's word.
	LatestBlockhash(ctx context.Context) (RecentBlockhash, error)
	SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error)
	SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, terminal ChainTerminal) (*TransactionOutcome, error)
}

// BuildSignSubmit assembles a single-signer transaction (the merchant is fee
// payer and sole required signer), signs its message via the per-merchant
// Signer, and submits it: the shared path for create_plan, update_plan and
// transfer_subscription.
//
// With one required signer, Signatures[0] is the fee payer's; a co-signer flow
// must order signatures to match the message's required-signer list.
func BuildSignSubmit(
	ctx context.Context,
	merchantID billing.MerchantID,
	signer Signer,
	rpc blockhashProvider,
	instructions []solanago.Instruction,
) (solanago.Signature, error) {
	return BuildSignSubmitPresubmit(ctx, merchantID, signer, rpc, instructions, nil)
}

// BuildSignSubmitPresubmit is BuildSignSubmit with a hook run after signing and
// before submission: the caller durably records the signature so a crash
// mid-submit resolves by a chain read, not a blind re-send. A presubmit error
// aborts the submit (nothing was sent).
func BuildSignSubmitPresubmit(
	ctx context.Context,
	merchantID billing.MerchantID,
	signer Signer,
	rpc blockhashProvider,
	instructions []solanago.Instruction,
	presubmit func(solanago.Signature) error,
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
	return BuildSignSubmitWithPayerPresubmit(ctx, rpc, payer, instructions, func(message []byte) (solanago.Signature, error) {
		return signer.SignMessage(ctx, merchantID, message)
	}, presubmit)
}

// BuildSignSubmitWithPayerPresubmit: see BuildSignSubmitPresubmit.
func BuildSignSubmitWithPayerPresubmit(
	ctx context.Context,
	rpc blockhashProvider,
	payer solanago.PublicKey,
	instructions []solanago.Instruction,
	signMessage func([]byte) (solanago.Signature, error),
	presubmit func(solanago.Signature) error,
) (solanago.Signature, error) {
	if signMessage == nil {
		return solanago.Signature{}, fmt.Errorf("solana: signer is required")
	}
	if rpc == nil {
		return solanago.Signature{}, fmt.Errorf("solana: rpc client is required")
	}
	if len(instructions) == 0 {
		return solanago.Signature{}, fmt.Errorf("solana: at least one instruction is required")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: get recent blockhash: %w", err)
	}

	tx, err := solanago.NewTransaction(instructions, blockhash.Hash, solanago.TransactionPayer(payer))
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: build transaction: %w", err)
	}

	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: serialize transaction message: %w", err)
	}

	sig, err := signMessage(msg)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: sign transaction: %w", err)
	}
	tx.Signatures = []solanago.Signature{sig}

	// Durable write-ahead of the signature: once persisted, a crash at any
	// later point resolves by reading the chain for it.
	if presubmit != nil {
		if err := presubmit(sig); err != nil {
			return solanago.Signature{}, fmt.Errorf("solana: presubmit persistence failed (transaction NOT sent): %w", err)
		}
	}

	// A pull is not success until it lands. A reverted tx comes back as the
	// outcome's on-chain error (with the program's Custom code) for the cranker
	// to classify. The watch ends on the blockhash's last valid height.
	outcome, err := rpc.SubmitAndConfirm(ctx, tx, blockhash.Terminal())
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: submit/confirm transaction: %w", err)
	}
	if oerr := outcome.OnChainError(); oerr != nil {
		return solanago.Signature{}, oerr
	}
	return outcome.Signature, nil
}
