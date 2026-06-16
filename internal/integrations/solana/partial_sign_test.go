package solana

import (
	"context"
	"encoding/base64"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type partialSignSecrets struct{ key string }

func (m partialSignSecrets) GetSecret(context.Context, merchant.ID, string) (string, error) {
	return m.key, nil
}

type zeroBlockhash struct{}

func (zeroBlockhash) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

// The cranker co-signs its slot; the wallet completes the other slot and the
// transaction is then fully + validly signed — proving the atomic co-signed
// tier-change tx (#272) round-trips across the wallet boundary.
func TestBuildPartiallySignedTx_WalletCompletes(t *testing.T) {
	merchant, err := solanago.NewRandomPrivateKey() // the cranker / co-signer
	require.NoError(t, err)
	subscriber, err := solanago.NewRandomPrivateKey() // the wallet / fee payer
	require.NoError(t, err)
	dest, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)

	cosigner := NewKeypairSigner(partialSignSecrets{key: merchant.String()}, 0)

	// A transaction requiring BOTH signers: a transfer from the subscriber (fee
	// payer) and one from the merchant — mirrors the upgrade bundle's two-signer
	// shape (subscriber signs cancel+subscribe, merchant signs the transfer caller).
	ixs := []solanago.Instruction{
		system.NewTransferInstruction(1, subscriber.PublicKey(), dest.PublicKey()).Build(),
		system.NewTransferInstruction(1, merchant.PublicKey(), dest.PublicKey()).Build(),
	}

	b64, err := BuildPartiallySignedTx(context.Background(), dbtest.TestMerchantID, cosigner, zeroBlockhash{}, subscriber.PublicKey(), ixs)
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(raw)
	require.NoError(t, err)

	require.Equal(t, 2, int(tx.Message.Header.NumRequiredSignatures), "two required signers")
	require.Len(t, tx.Signatures, 2)
	require.True(t, subscriber.PublicKey().Equals(tx.Message.AccountKeys[0]), "fee payer (subscriber) is signer 0")
	require.True(t, tx.Signatures[0].IsZero(), "wallet slot left empty for the user to complete")
	require.False(t, tx.Signatures[1].IsZero(), "cranker slot is pre-signed")
	require.Error(t, tx.VerifySignatures(), "not yet valid — wallet hasn't signed")

	// Wallet completes: sign the message and fill the fee-payer slot (index 0).
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	subSig, err := subscriber.Sign(msg)
	require.NoError(t, err)
	tx.Signatures[0] = subSig

	require.NoError(t, tx.VerifySignatures(), "fully + validly signed after the wallet completes it")
}

func TestBuildPartiallySignedTx_RejectsNonSigner(t *testing.T) {
	merchant, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	subscriber, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	dest, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	cosigner := NewKeypairSigner(partialSignSecrets{key: merchant.String()}, 0)

	// Instruction does NOT require the merchant to sign -> cosigner isn't a signer.
	ixs := []solanago.Instruction{
		system.NewTransferInstruction(1, subscriber.PublicKey(), dest.PublicKey()).Build(),
	}
	_, err = BuildPartiallySignedTx(context.Background(), dbtest.TestMerchantID, cosigner, zeroBlockhash{}, subscriber.PublicKey(), ixs)
	require.Error(t, err, "must reject when the cosigner is not a required signer")
}
