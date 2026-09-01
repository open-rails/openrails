package solana

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/merchant"
)

const privateKeySecretName = "private_key"

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
//   - keypairSigner: loads the PSP scoped private_key secret and
//     signs in-process. Works with any secret backend; the key briefly lives in
//     memory.
//   - transitSigner: signs through Vault Transit — the key never leaves Vault.
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
	// LatestBlockhash returns the blockhash WITH its chain terminal, so the
	// confirmation watch ends on the chain's word (xs-007 row 36).
	LatestBlockhash(ctx context.Context) (RecentBlockhash, error)
	SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error)
	SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, terminal ChainTerminal) (*TransactionOutcome, error)
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
	return BuildSignSubmitPresubmit(ctx, merchantID, signer, rpc, instructions, nil)
}

// BuildSignSubmitPresubmit is BuildSignSubmit with a persistence hook invoked
// AFTER signing and BEFORE submission: the caller durably records the tx
// signature (#674) so a crash mid-submit resolves via a chain read instead of
// a blind re-send. A presubmit error aborts the submit (nothing was sent).
func BuildSignSubmitPresubmit(
	ctx context.Context,
	merchantID merchant.ID,
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

	// Durable write-ahead of the signature (#674): once persisted, a crash at
	// any later point is resolvable by reading the chain for this signature.
	if presubmit != nil {
		if err := presubmit(sig); err != nil {
			return solanago.Signature{}, fmt.Errorf("solana: presubmit persistence failed (transaction NOT sent): %w", err)
		}
	}

	// Submit AND confirm: a billing pull must not be treated as success until the
	// transaction has actually landed. SubmitAndConfirm surfaces a reverted tx via
	// the outcome's on-chain error (with the program's Custom code) so the cranker
	// can classify it (#270) instead of silently "succeeding" on a failed pull.
	// The watch ends on the blockhash's own last valid height, never a clock.
	outcome, err := rpc.SubmitAndConfirm(ctx, tx, blockhash.Terminal())
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solana: submit/confirm transaction: %w", err)
	}
	if oerr := outcome.OnChainError(); oerr != nil {
		return solanago.Signature{}, oerr
	}
	return outcome.Signature, nil
}
