package solana

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

// #713: memos are public and immutable; recovery tooling parses these bytes.
func TestPurchaseMemoWireAndParse(t *testing.T) {
	id := uuid.MustParse("0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42")
	require.Equal(t, "openrails:1:0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42", PurchaseMemo(id))

	got, ok := ParsePurchaseMemo("  " + PurchaseMemo(id) + "\n")
	require.True(t, ok)
	require.Equal(t, id, got)
	for _, memo := range []string{
		"", "gm", "openrails:1:", "openrails:2:" + id.String(), "openrails:" + id.String(),
		"OpenRails:1:" + id.String(), "openrails:1:0dae1b8f4c6e4f6a9b2d7e5c3a1f8d42",
		"openrails:1:" + uuid.Nil.String(), "openrails:1:" + id.String() + "x",
	} {
		got, ok := ParsePurchaseMemo(memo)
		require.False(t, ok, memo)
		require.Equal(t, uuid.Nil, got, memo)
	}

	ix := NewMemoInstruction(PurchaseMemo(id))
	require.Equal(t, "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr", ix.ProgramID().String())
	data, err := ix.Data()
	require.NoError(t, err)
	require.Equal(t, []byte(PurchaseMemo(id)), data, "raw string, no length prefix")
	require.Empty(t, ix.Accounts())
}

// Solana Pay ordering: the memo precedes the transfer, for SOL and SPL; an SPL
// transfer is a TransferChecked under the mint's own token program, into the
// token accounts that program derives.
func TestTransferInstructionsPutMemoFirst(t *testing.T) {
	from, to := solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey()
	mint := solanago.NewWallet().PublicKey()
	memo := PurchaseMemo(uuid.New())
	for _, c := range []struct {
		symbol  string
		mint    *MintInfo
		program solanago.PublicKey
	}{
		{"SOL", nil, system.ProgramID},
		{"USDC", &MintInfo{Mint: mint, Program: solanago.TokenProgramID, Decimals: 6}, solanago.TokenProgramID},
		{"PYUSD", &MintInfo{Mint: mint, Program: Token2022ProgramID, Decimals: 6}, Token2022ProgramID},
	} {
		req := TransferRequest{TokenSymbol: c.symbol, TokenMint: mint.String(), Amount: 5, Memo: memo}
		ixs, err := buildTransferInstructions(req, from, to, c.mint)
		require.NoError(t, err)
		require.Len(t, ixs, 2)
		require.Equal(t, solanago.MemoProgramID, ixs[0].ProgramID())
		require.Equal(t, c.program, ixs[1].ProgramID(), c.symbol)
		if c.mint != nil {
			dest, err := AssociatedTokenAddress(to, mint, c.program)
			require.NoError(t, err)
			require.Equal(t, dest, ixs[1].Accounts()[2].PublicKey, "%s pays the recipient's account under its program", c.symbol)
			require.Equal(t, mint, ixs[1].Accounts()[1].PublicKey)
		}

		req.Memo = ""
		ixs, err = buildTransferInstructions(req, from, to, c.mint)
		require.NoError(t, err)
		require.Len(t, ixs, 1)
	}
}

// or#893: a present memo must match; absence fails only when we built the tx.
func TestVerifyPurchaseMemoPolicy(t *testing.T) {
	payer, dest := solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey()
	want, other := uuid.New(), uuid.New()
	tx := func(leading ...solanago.Instruction) *solanago.Transaction {
		out, err := solanago.NewTransaction(append(leading, system.NewTransferInstruction(1, payer, dest).Build()), solanago.Hash{}, solanago.TransactionPayer(payer))
		require.NoError(t, err)
		return out
	}
	stamped := tx(NewMemoInstruction(PurchaseMemo(want)))
	bare := tx()
	foreign := tx(NewMemoInstruction("gm"))

	require.Equal(t, []uuid.UUID{want}, PurchaseMemoLocalIDs(stamped))
	require.Empty(t, PurchaseMemoLocalIDs(foreign))
	require.Empty(t, PurchaseMemoLocalIDs(nil))

	for _, c := range []struct {
		tx      *solanago.Transaction
		want    uuid.UUID
		policy  PurchaseMemoPolicy
		wantErr string
	}{
		{stamped, want, MemoRequired, ""},
		{stamped, want, MemoPresenceOptional, ""},
		{stamped, other, MemoPresenceOptional, "purchase memo mismatch"},
		{stamped, other, MemoRequired, "purchase memo mismatch"},
		{bare, want, MemoPresenceOptional, ""},
		{bare, want, MemoRequired, "purchase memo missing"},
		{foreign, want, MemoPresenceOptional, ""},
		{foreign, want, MemoRequired, "purchase memo missing"},
		{stamped, uuid.Nil, MemoRequired, ""},
	} {
		err := VerifyPurchaseMemo(c.tx, c.want, c.policy)
		if c.wantErr == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, c.wantErr)
		}
	}
}

func mintBlob(decimals uint8, initialized bool) []byte {
	b := make([]byte, MintAccountSize)
	b[0] = 1
	b[36] = 0xFF // supply byte next to decimals
	b[44] = decimals
	if initialized {
		b[45] = 1
	}
	return b
}

type mintReader struct {
	data []byte
	err  error
}

func (r mintReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	return r.data, r.err
}

// SPL mint layout: decimals @44, is_initialized @45, 82-byte base (Token-2022
// appends TLV). Decimals are chain truth: nothing undecodable defaults to 0.
func TestMintDecimalsFailClosed(t *testing.T) {
	require.Equal(t, 82, MintAccountSize)
	for _, d := range []uint8{0, 6, 9, 18, 255} {
		got, err := DecodeMintDecimals(mintBlob(d, true))
		require.NoError(t, err)
		require.Equal(t, int(d), got)
	}
	got, err := DecodeMintDecimals(append(mintBlob(9, true), make([]byte, 200)...))
	require.NoError(t, err)
	require.Equal(t, 9, got)
	for _, bad := range [][]byte{nil, make([]byte, MintAccountSize-1), mintBlob(6, false)} {
		_, err := DecodeMintDecimals(bad)
		require.Error(t, err)
	}

	mint := solanago.NewWallet().PublicKey()
	ctx := context.Background()
	got, err = ReadMintDecimals(ctx, mintReader{data: mintBlob(6, true)}, mint)
	require.NoError(t, err)
	require.Equal(t, 6, got)
	_, err = ReadMintDecimals(ctx, mintReader{}, mint)
	require.ErrorIs(t, err, ErrMintAccountNotFound)
	_, err = ReadMintDecimals(ctx, mintReader{err: errors.New("rpc down")}, mint)
	require.Error(t, err)
	_, err = ReadMintDecimals(ctx, nil, mint)
	require.Error(t, err)
}

type countingSecrets struct {
	mu    sync.Mutex
	value string
	err   error
	reads int
}

func (s *countingSecrets) GetSecret(context.Context, merchant.ID, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.value, s.err
}

type fakeTransit struct {
	mu              sync.Mutex
	key             solanago.PrivateKey
	err             error
	pubLen, sigLen  int
	signs, pubReads int
	names           []string
}

func (f *fakeTransit) Sign(_ context.Context, name string, input []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signs++
	f.names = append(f.names, name)
	if f.err != nil {
		return nil, f.err
	}
	sig, err := f.key.Sign(input)
	if err != nil {
		return nil, err
	}
	return sig[:f.sigLen], nil
}

func (f *fakeTransit) PublicKey(_ context.Context, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubReads++
	f.names = append(f.names, name)
	if f.err != nil {
		return nil, f.err
	}
	pk := f.key.PublicKey()
	return pk[:f.pubLen], nil
}

// Both signers sign verifiably for the merchant key and fail closed on a zero
// merchant, an empty message, or a broken key store.
func TestSignersFailClosed(t *testing.T) {
	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mid := merchant.ID(uuid.New())
	ctx := context.Background()
	signers := map[string]func(broken bool) Signer{
		"keypair": func(broken bool) Signer {
			s := &countingSecrets{value: key.String()}
			if broken {
				s.err = errors.New("vault unreachable")
			}
			return NewKeypairSigner(s, time.Minute)
		},
		"transit": func(broken bool) Signer {
			f := &fakeTransit{key: key, pubLen: 32, sigLen: 64}
			if broken {
				f.err = errors.New("transit unreachable")
			}
			return NewTransitSigner(f, nil, time.Minute)
		},
	}
	for name, mk := range signers {
		t.Run(name, func(t *testing.T) {
			s := mk(false)
			pub, err := s.PublicKey(ctx, mid)
			require.NoError(t, err)
			require.Equal(t, key.PublicKey(), pub)
			msg := []byte("transfer_subscription message")
			sig, err := s.SignMessage(ctx, mid, msg)
			require.NoError(t, err)
			require.True(t, sig.Verify(pub, msg))

			_, err = s.SignMessage(ctx, merchant.ID{}, msg)
			require.Error(t, err, "zero merchant")
			_, err = s.SignMessage(ctx, mid, nil)
			require.Error(t, err, "empty message")
			_, err = mk(true).SignMessage(ctx, mid, msg)
			require.Error(t, err, "store failure")
		})
	}

	t.Run("transit rejects malformed key material", func(t *testing.T) {
		_, err := NewTransitSigner(&fakeTransit{key: key, pubLen: 31, sigLen: 64}, nil, time.Minute).PublicKey(ctx, mid)
		require.Error(t, err)
		_, err = NewTransitSigner(&fakeTransit{key: key, pubLen: 32, sigLen: 63}, nil, time.Minute).SignMessage(ctx, mid, []byte("m"))
		require.Error(t, err)
	})
}

// Keys are cached per TTL window; transit caches only the public key — every
// signature round-trips Vault, under the configured key name.
func TestSignerCaching(t *testing.T) {
	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	mid := merchant.ID(uuid.New())
	ctx := context.Background()

	secrets := &countingSecrets{value: key.String()}
	ks := NewKeypairSigner(secrets, time.Minute).(*keypairSigner)
	now := time.Unix(1_700_000_000, 0)
	ks.now = func() time.Time { return now }
	for _, step := range []time.Duration{0, 30 * time.Second, time.Minute} {
		now = now.Add(step)
		_, err := ks.PublicKey(ctx, mid)
		require.NoError(t, err)
	}
	require.Equal(t, 2, secrets.reads, "one read per TTL window")

	ft := &fakeTransit{key: key, pubLen: 32, sigLen: 64}
	ts := NewTransitSigner(ft, func(id merchant.ID) string { return "custom-" + id.String() }, time.Minute)
	for range 3 {
		_, err := ts.PublicKey(ctx, mid)
		require.NoError(t, err)
		_, err = ts.SignMessage(ctx, mid, []byte("m"))
		require.NoError(t, err)
	}
	require.Equal(t, 1, ft.pubReads)
	require.Equal(t, 3, ft.signs)
	for _, n := range ft.names {
		require.Equal(t, "custom-"+mid.String(), n)
	}
}

type fixedBlockhash struct{}

func (fixedBlockhash) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{3}, nil
}

// #272: the co-signer fills only its own slot; the wallet's slot stays empty
// until the wallet signs, and a non-signer co-signer is refused.
func TestBuildPartiallySignedTx(t *testing.T) {
	cranker, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	wallet, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	dest := solanago.NewWallet().PublicKey()
	cosigner := NewKeypairSigner(staticSecret(cranker.String()), 0)
	mid := merchant.ID(uuid.New())
	ctx := context.Background()

	ixs := []solanago.Instruction{
		system.NewTransferInstruction(1, wallet.PublicKey(), dest).Build(),
		system.NewTransferInstruction(1, cranker.PublicKey(), dest).Build(),
	}
	b64, err := BuildPartiallySignedTx(ctx, mid, cosigner, fixedBlockhash{}, wallet.PublicKey(), ixs)
	require.NoError(t, err)
	raw, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(raw)
	require.NoError(t, err)

	require.EqualValues(t, 2, tx.Message.Header.NumRequiredSignatures)
	require.Len(t, tx.Signatures, 2)
	require.True(t, wallet.PublicKey().Equals(tx.Message.AccountKeys[0]), "wallet is fee payer")
	require.True(t, tx.Signatures[0].IsZero())
	require.False(t, tx.Signatures[1].IsZero())
	require.Error(t, tx.VerifySignatures())

	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	tx.Signatures[0], err = wallet.Sign(msg)
	require.NoError(t, err)
	require.NoError(t, tx.VerifySignatures())

	_, err = BuildPartiallySignedTx(ctx, mid, cosigner, fixedBlockhash{}, wallet.PublicKey(), ixs[:1])
	require.ErrorContains(t, err, "not a required signer")
}

// Read-after-write compensation: poll until the predicate holds, retry
// transient errors, return the last value on exhaustion, honour ctx.
func TestReadUntilConsistent(t *testing.T) {
	positive := func(v uint64) bool { return v > 0 }
	script := func(vals []uint64, errs []error) (func(context.Context) (uint64, error), *int) {
		n := 0
		return func(context.Context) (uint64, error) {
			i := min(n, len(vals)-1)
			n++
			return vals[i], errs[i]
		}, &n
	}
	opts := ReadUntilConsistentOpts{Attempts: 4, Backoff: time.Millisecond}

	read, n := script([]uint64{0, 0, 5}, []error{nil, nil, nil})
	got, err := ReadUntilConsistent(context.Background(), opts, read, positive)
	require.NoError(t, err)
	require.Equal(t, uint64(5), got)
	require.Equal(t, 3, *n)

	read, n = script([]uint64{7}, []error{nil})
	_, err = ReadUntilConsistent(context.Background(), ReadUntilConsistentOpts{Attempts: 5, Backoff: time.Hour}, read, positive)
	require.NoError(t, err, "first success incurs no backoff")
	require.Equal(t, 1, *n)

	read, _ = script([]uint64{0, 9}, []error{errors.New("could not find account"), nil})
	got, err = ReadUntilConsistent(context.Background(), opts, read, positive)
	require.NoError(t, err)
	require.Equal(t, uint64(9), got)

	read, n = script([]uint64{0}, []error{nil})
	got, err = ReadUntilConsistent(context.Background(), opts, read, positive)
	require.ErrorContains(t, err, "never satisfied")
	require.Zero(t, got)
	require.Equal(t, 4, *n)

	sentinel := errors.New("rpc down")
	read, _ = script([]uint64{0}, []error{sentinel})
	_, err = ReadUntilConsistent(context.Background(), opts, read, positive)
	require.ErrorIs(t, err, sentinel)

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err = ReadUntilConsistent(ctx, ReadUntilConsistentOpts{Attempts: 10, Backoff: 50 * time.Millisecond},
		func(context.Context) (uint64, error) { calls++; cancel(); return 0, nil }, positive)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)

	require.Equal(t, defaultReadUntilConsistentAttempts, ReadUntilConsistentOpts{}.attempts())
	require.Equal(t, defaultReadUntilConsistentBackoff, ReadUntilConsistentOpts{}.backoff())
}

// A lagging node's min-context-slot error is retried; any other error is not.
func TestMinContextSlotRetry(t *testing.T) {
	for msg, want := range map[string]bool{
		"Minimum context slot has not been reached":                     true,
		"minimum context slot has not been reached: served=10 want>=20": true,
		"Node is behind by 100 slots":                                   false,
		"could not find account":                                        false,
	} {
		require.Equal(t, want, isMinContextSlotError(errors.New(msg)), msg)
	}
	require.False(t, isMinContextSlotError(nil))

	calls := 0
	err := retryMinContextSlotWithBackoff(context.Background(), time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("minimum context slot has not been reached")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, calls)

	sentinel := errors.New("invalid public key")
	calls = 0
	err = retryMinContextSlot(context.Background(), func(context.Context) error { calls++; return sentinel })
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, calls)
}

func TestValidateAddressAndSignature(t *testing.T) {
	var maxSig solanago.Signature
	for i := range maxSig {
		maxSig[i] = 0xFF
	}
	// Any 64-byte value is a well-formed signature; appending to the max value overflows it.
	sig88 := maxSig.String()
	for s, ok := range map[string]bool{
		"11111111111111111111111111111112":            true,
		"So11111111111111111111111111111111111111112": true,
		"":                                   false,
		"111111111111111111111111111111":     false,
		"1111111111111111111111111111111O":   false,
		"1111111111111111111111111111111110": false,
	} {
		require.Equal(t, ok, ValidateAddress(s) == nil, s)
	}
	for s, ok := range map[string]bool{
		sig88:                      true,
		"":                         false,
		sig88[:len(sig88)-3]:       false,
		sig88 + "2":                false,
		sig88[:len(sig88)-1] + "0": false,
	} {
		require.Equal(t, ok, ValidateSignature(s) == nil, s)
	}
	require.ErrorIs(t, ValidateAddress(""), ErrInvalidAddress)
	require.ErrorIs(t, ValidateSignature(""), ErrInvalidSignature)
}

// A TransferChecked carries the mint's decimals as one byte; a value outside
// it is refused, never truncated.
func TestMintDecimalsFitOneByte(t *testing.T) {
	for _, ok := range []int{0, 6, 9, 255} {
		d, err := mintDecimals(ok)
		require.NoError(t, err)
		require.Equal(t, uint8(ok), d)
	}
	for _, bad := range []int{-1, 256, 1 << 20} {
		_, err := mintDecimals(bad)
		require.ErrorContains(t, err, "outside 0..255", bad)
	}
}

func token2022Mint(t *testing.T, fee *[2]TransferFee, hook solanago.PublicKey) []byte {
	t.Helper()
	data := make([]byte, token2022AccountTypeOffset+1)
	data[44], data[45] = 6, 1
	data[token2022AccountTypeOffset] = token2022AccountTypeMint
	tlv := func(typ uint16, value []byte) {
		head := make([]byte, 4)
		binary.LittleEndian.PutUint16(head, typ)
		binary.LittleEndian.PutUint16(head[2:], uint16(len(value)))
		data = append(append(data, head...), value...)
	}
	if fee != nil {
		v := make([]byte, transferFeeConfigLen)
		for i, f := range fee {
			off := 72 + 18*i
			binary.LittleEndian.PutUint64(v[off:], f.Epoch)
			binary.LittleEndian.PutUint64(v[off+8:], f.MaximumFee)
			binary.LittleEndian.PutUint16(v[off+16:], f.BasisPts)
		}
		tlv(extensionTransferFeeConfig, v)
	}
	if !hook.IsZero() {
		tlv(extensionTransferHook, append(make([]byte, 32), hook[:]...))
	}
	return data
}

// A Token-2022 transfer fee is read at the current epoch; the payer sends the
// smallest gross that delivers the quoted net, and a transfer hook is seen.
func TestToken2022FeeAndHook(t *testing.T) {
	fees := [2]TransferFee{{Epoch: 0, MaximumFee: 1_000_000, BasisPts: 100}, {Epoch: 10, MaximumFee: 5, BasisPts: 200}}
	fee, hook, err := mintExtensions(token2022Mint(t, &fees, solanago.PublicKey{}), 9)
	require.NoError(t, err)
	require.Equal(t, fees[0], *fee, "the older fee until its successor's epoch")
	require.True(t, hook.IsZero())
	fee, _, err = mintExtensions(token2022Mint(t, &fees, solanago.PublicKey{}), 10)
	require.NoError(t, err)
	require.Equal(t, fees[1], *fee)

	onePercent := TransferFee{MaximumFee: 1_000_000, BasisPts: 100}
	for net, want := range map[uint64][2]uint64{100: {102, 2}, 5_000_000: {5_050_506, 50_506}, 1: {2, 1}} {
		gross, f, err := onePercent.GrossFor(net)
		require.NoError(t, err)
		require.Equal(t, want, [2]uint64{gross, f}, net)
		require.GreaterOrEqual(t, gross-onePercent.For(gross), net)
		require.Less(t, gross-1-onePercent.For(gross-1), net, "the smallest gross")
	}
	capped := TransferFee{MaximumFee: 5, BasisPts: 200}
	gross, f, err := capped.GrossFor(1_000_000)
	require.NoError(t, err)
	require.Equal(t, [2]uint64{1_000_005, 5}, [2]uint64{gross, f}, "the fee is capped at its maximum")

	program := solanago.NewWallet().PublicKey()
	_, hook, err = mintExtensions(token2022Mint(t, nil, program), 0)
	require.NoError(t, err)
	require.Equal(t, program, hook)

	legacy, hook, err := mintExtensions(make([]byte, MintAccountSize), 0)
	require.NoError(t, err)
	require.Nil(t, legacy)
	require.True(t, hook.IsZero())
	_, _, err = mintExtensions(append(token2022Mint(t, nil, solanago.PublicKey{}), 1, 0, 200, 0), 0)
	require.ErrorContains(t, err, "overruns")
}

// Under a transfer fee the built transaction sends the gross with the fee
// asserted on-chain (TransferCheckedWithFee).
func TestTransferWithFeeAssertsTheFee(t *testing.T) {
	from, to, mint := solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey(), solanago.NewWallet().PublicKey()
	info := &MintInfo{Mint: mint, Program: Token2022ProgramID, Decimals: 6, Fee: &TransferFee{MaximumFee: 1_000_000, BasisPts: 100}}
	ixs, err := buildTransferInstructions(TransferRequest{TokenSymbol: "PYUSD", TokenMint: mint.String(), Amount: 100}, from, to, info)
	require.NoError(t, err)
	require.Equal(t, Token2022ProgramID, ixs[0].ProgramID())
	data, err := ixs[0].Data()
	require.NoError(t, err)
	require.Equal(t, []byte{26, 1}, data[:2])
	require.Equal(t, uint64(102), binary.LittleEndian.Uint64(data[2:]), "gross")
	require.Equal(t, byte(6), data[10])
	require.Equal(t, uint64(2), binary.LittleEndian.Uint64(data[11:]), "asserted fee")
}
