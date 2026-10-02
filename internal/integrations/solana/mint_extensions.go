package solana

import (
	"encoding/binary"
	"fmt"
	"math/big"

	solanago "github.com/gagliardetto/solana-go"
)

// Token-2022 mint layout: the 82-byte base mint, padding to the account-type
// byte at 165, then TLV extensions (type u16, length u16, value).
const (
	token2022AccountTypeOffset = 165
	token2022AccountTypeMint   = 1
	extensionTransferFeeConfig = 1
	extensionTransferHook      = 14
	transferFeeConfigLen       = 108
	transferHookLen            = 64
	maxFeeBasisPoints          = 10_000
)

// TransferFee is the fee a Token-2022 mint withholds from every transfer at
// one epoch: basis points of the amount, capped at MaximumFee.
type TransferFee struct {
	Epoch      uint64
	MaximumFee uint64
	BasisPts   uint16
}

// For is the fee withheld from a transfer of gross units.
func (f TransferFee) For(gross uint64) uint64 {
	if f.BasisPts == 0 || gross == 0 {
		return 0
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(gross), big.NewInt(int64(f.BasisPts)))
	n.Add(n, big.NewInt(maxFeeBasisPoints-1))
	n.Quo(n, big.NewInt(maxFeeBasisPoints))
	if !n.IsUint64() || n.Uint64() > f.MaximumFee {
		return f.MaximumFee
	}
	return n.Uint64()
}

// GrossFor is the smallest transfer that delivers at least net after the fee,
// and that fee.
func (f TransferFee) GrossFor(net uint64) (uint64, uint64, error) {
	gross := net
	for range 64 {
		fee := f.For(gross)
		if gross-fee >= net {
			return gross, fee, nil
		}
		next := net + fee
		if next < net {
			return 0, 0, fmt.Errorf("solana: transfer of %d plus fee overflows", net)
		}
		gross = next
	}
	return 0, 0, fmt.Errorf("solana: transfer fee for %d did not settle", net)
}

// mintExtensions reads the transfer fee in force at epoch and whether a
// transfer hook program is set, from a Token-2022 mint's data. A legacy mint
// has neither.
func mintExtensions(data []byte, epoch uint64) (*TransferFee, solanago.PublicKey, error) {
	if len(data) <= token2022AccountTypeOffset {
		return nil, solanago.PublicKey{}, nil
	}
	if data[token2022AccountTypeOffset] != token2022AccountTypeMint {
		return nil, solanago.PublicKey{}, fmt.Errorf("solana: account type %d is not a mint", data[token2022AccountTypeOffset])
	}
	var fee *TransferFee
	var hook solanago.PublicKey
	for off := token2022AccountTypeOffset + 1; off+4 <= len(data); {
		typ := binary.LittleEndian.Uint16(data[off:])
		size := int(binary.LittleEndian.Uint16(data[off+2:]))
		value := data[off+4:]
		if size > len(value) {
			return nil, solanago.PublicKey{}, fmt.Errorf("solana: mint extension %d overruns the account", typ)
		}
		value = value[:size]
		switch typ {
		case extensionTransferFeeConfig:
			if size != transferFeeConfigLen {
				return nil, solanago.PublicKey{}, fmt.Errorf("solana: transfer fee config is %d bytes", size)
			}
			older, newer := decodeTransferFee(value[72:90]), decodeTransferFee(value[90:108])
			current := older
			if epoch >= newer.Epoch {
				current = newer
			}
			if current.BasisPts > maxFeeBasisPoints {
				return nil, solanago.PublicKey{}, fmt.Errorf("solana: transfer fee of %d basis points", current.BasisPts)
			}
			fee = &current
		case extensionTransferHook:
			if size != transferHookLen {
				return nil, solanago.PublicKey{}, fmt.Errorf("solana: transfer hook is %d bytes", size)
			}
			hook = solanago.PublicKeyFromBytes(value[32:64])
		}
		if typ == 0 && size == 0 {
			break
		}
		off += 4 + size
	}
	return fee, hook, nil
}

func decodeTransferFee(b []byte) TransferFee {
	return TransferFee{Epoch: binary.LittleEndian.Uint64(b), MaximumFee: binary.LittleEndian.Uint64(b[8:]), BasisPts: binary.LittleEndian.Uint16(b[16:])}
}

// transferCheckedWithFee is Token-2022's TransferFeeExtension/TransferCheckedWithFee:
// the fee is asserted on-chain, so a fee that changed since it was computed
// fails the transfer instead of delivering less.
func transferCheckedWithFee(program, source, mint, destination, authority solanago.PublicKey, amount uint64, decimals uint8, fee uint64) solanago.Instruction {
	data := make([]byte, 19)
	data[0], data[1] = 26, 1
	binary.LittleEndian.PutUint64(data[2:], amount)
	data[10] = decimals
	binary.LittleEndian.PutUint64(data[11:], fee)
	return solanago.NewInstruction(program, []*solanago.AccountMeta{
		solanago.Meta(source).WRITE(), solanago.Meta(mint), solanago.Meta(destination).WRITE(), solanago.Meta(authority).SIGNER(),
	}, data)
}
