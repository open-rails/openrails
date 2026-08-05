package solana

import (
	"context"
	"errors"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
)

// SPL Token mint account, base layout (82 bytes):
//
//	[0:36)  mint_authority   COption<Pubkey>
//	[36:44) supply           u64 (LE)
//	[44]    decimals         u8
//	[45]    is_initialized   bool
//	[46:82) freeze_authority COption<Pubkey>
//
// Token-2022 keeps this exact base layout and appends TLV extensions after byte
// 82, so the decimals byte sits at the same offset under both programs.
//
// `decimals` is written once by InitializeMint and there is no instruction that
// changes it — it is IMMUTABLE for the life of the mint. That is what makes the
// value cacheable (see modules/solana.MintDecimals) and what makes a plain
// GetAccountData read safe here: this is never a read-after-our-own-write, so
// there is no slot to gate on. Any read that DOES follow a confirmed
// transaction must still use GetAccountDataAtSlot / ReadUntilConsistent.
const (
	MintAccountSize         = 82
	mintDecimalsOffset      = 44
	mintIsInitializedOffset = 45
)

// ErrMintAccountNotFound: the mint address holds no account. Fail closed — a
// missing mint is never a reason to assume a decimals value.
var ErrMintAccountNotFound = errors.New("solana: mint account not found on chain")

// DecodeMintDecimals reads the base-unit precision out of raw SPL mint account
// data. It refuses anything that is not a decodable, initialized mint rather
// than returning a guessable zero.
func DecodeMintDecimals(data []byte) (int, error) {
	if len(data) < MintAccountSize {
		return 0, fmt.Errorf("solana: mint account data is %d bytes, want >= %d", len(data), MintAccountSize)
	}
	if data[mintIsInitializedOffset] == 0 {
		return 0, errors.New("solana: mint account is not initialized")
	}
	return int(data[mintDecimalsOffset]), nil
}

// MintAccountReader is the read surface ReadMintDecimals needs. Satisfied by
// *RPCClient and by the merchant-scoped chain readers.
type MintAccountReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// ReadMintDecimals returns the mint's on-chain base-unit precision. The chain is
// the source of truth for decimals: an unreadable, absent, or undecodable mint
// is an error, never a default.
func ReadMintDecimals(ctx context.Context, reader MintAccountReader, mint solanago.PublicKey) (int, error) {
	if reader == nil {
		return 0, fmt.Errorf("solana: no chain reader armed to read mint %s decimals", mint)
	}
	data, err := reader.GetAccountData(ctx, mint)
	if err != nil {
		return 0, fmt.Errorf("solana: read mint %s: %w", mint, err)
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("%w: %s", ErrMintAccountNotFound, mint)
	}
	decimals, err := DecodeMintDecimals(data)
	if err != nil {
		return 0, fmt.Errorf("%w (mint %s)", err, mint)
	}
	return decimals, nil
}

// GetMintDecimals reads a mint's on-chain decimals through this client.
func (c *RPCClient) GetMintDecimals(ctx context.Context, mint solanago.PublicKey) (int, error) {
	return ReadMintDecimals(ctx, c, mint)
}
