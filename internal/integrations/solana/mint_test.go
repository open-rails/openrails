package solana

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
)

// buildMint assembles an SPL Token mint account in the program's exact base
// layout. Field offsets are pinned here so a layout regression fails loudly
// rather than silently returning some other byte as "decimals".
func buildMint(decimals uint8, initialized bool) []byte {
	blob := make([]byte, MintAccountSize)
	blob[0] = 1 // mint_authority COption tag = Some
	copy(blob[4:36], make([]byte, 32))
	blob[36] = 0xFF // supply (LE) — irrelevant to decimals
	blob[mintDecimalsOffset] = decimals
	if initialized {
		blob[mintIsInitializedOffset] = 1
	}
	return blob
}

// TestMintLayoutOffsets pins the SPL mint layout constants. decimals lives at
// byte 44 (36 bytes mint_authority COption + 8 bytes supply) and the account is
// 82 bytes. Token-2022 keeps this base layout and appends TLV extensions.
func TestMintLayoutOffsets(t *testing.T) {
	if MintAccountSize != 82 {
		t.Fatalf("MintAccountSize = %d, want 82", MintAccountSize)
	}
	if mintDecimalsOffset != 44 {
		t.Fatalf("mintDecimalsOffset = %d, want 44", mintDecimalsOffset)
	}
	if mintIsInitializedOffset != 45 {
		t.Fatalf("mintIsInitializedOffset = %d, want 45", mintIsInitializedOffset)
	}
}

// TestDecodeMintDecimals pins the decode for every precision that matters:
// 6 (USDC), 9 (wrapped SOL and many SPL tokens), and the extremes.
func TestDecodeMintDecimals(t *testing.T) {
	for _, want := range []uint8{0, 2, 6, 8, 9, 18, 255} {
		got, err := DecodeMintDecimals(buildMint(want, true))
		if err != nil {
			t.Fatalf("DecodeMintDecimals(decimals=%d): %v", want, err)
		}
		if got != int(want) {
			t.Fatalf("DecodeMintDecimals = %d, want %d", got, want)
		}
	}
}

// A Token-2022 mint carries TLV extensions after the 82-byte base; the decimals
// byte must still decode from the same offset.
func TestDecodeMintDecimals_Token2022TrailingExtensions(t *testing.T) {
	blob := append(buildMint(9, true), make([]byte, 200)...)
	got, err := DecodeMintDecimals(blob)
	if err != nil || got != 9 {
		t.Fatalf("DecodeMintDecimals(token-2022) = (%d, %v), want (9, nil)", got, err)
	}
}

// Fail closed: nothing here may yield a guessable zero.
func TestDecodeMintDecimals_RefusesUndecodable(t *testing.T) {
	if _, err := DecodeMintDecimals(nil); err == nil {
		t.Fatal("nil data must error, not decode")
	}
	if _, err := DecodeMintDecimals(make([]byte, MintAccountSize-1)); err == nil {
		t.Fatal("short data must error, not decode")
	}
	if _, err := DecodeMintDecimals(buildMint(6, false)); err == nil {
		t.Fatal("uninitialized mint must error, not decode")
	}
}

type stubReader struct {
	data  []byte
	err   error
	calls int
}

func (r *stubReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	r.calls++
	return r.data, r.err
}

func TestReadMintDecimals(t *testing.T) {
	mint := solanago.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	ctx := context.Background()

	got, err := ReadMintDecimals(ctx, &stubReader{data: buildMint(9, true)}, mint)
	if err != nil || got != 9 {
		t.Fatalf("ReadMintDecimals = (%d, %v), want (9, nil)", got, err)
	}

	// An account that does not exist is an error, never a default.
	_, err = ReadMintDecimals(ctx, &stubReader{}, mint)
	if !errors.Is(err, ErrMintAccountNotFound) {
		t.Fatalf("absent mint error = %v, want ErrMintAccountNotFound", err)
	}

	// A transport failure is an error, never a default.
	if _, err := ReadMintDecimals(ctx, &stubReader{err: errors.New("rpc down")}, mint); err == nil {
		t.Fatal("rpc failure must surface, not fall back to a default")
	}

	// No reader armed at all is an error.
	if _, err := ReadMintDecimals(ctx, nil, mint); err == nil {
		t.Fatal("nil reader must error")
	}
}
