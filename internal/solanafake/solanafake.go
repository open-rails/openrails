// Package solanafake is a loopback Solana JSON-RPC node for sandbox tests. It
// serves the accounts a test declares — SPL mints and published subscription
// plans — and the transfers a test lands, to an OpenRails whose
// provider_sandbox.solana_rpc_url points at it.
package solanafake

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// Devnet mints OpenRails' token registry pins.
const (
	DevnetSOLMint  = "So11111111111111111111111111111111111111112"
	DevnetUSDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	DevnetDUSDMint = "7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T"
)

type Node struct {
	server   *httptest.Server
	mu       sync.Mutex
	accounts map[string][]byte
	// landed transactions by signature, and each address's signatures in
	// landing order.
	txs    map[string]landed
	byAddr map[string][]string
	height uint64
}

type landed struct {
	raw       []byte
	meta      map[string]any
	blockTime time.Time
	slot      uint64
}

// Transfer is one wallet payment a test lands on the node.
type Transfer struct {
	Payer     solanago.PublicKey
	Recipient string // wallet
	Mint      string // SPL mint; empty or the SOL mint pays lamports
	Amount    uint64
	Reference string
	// Also names further references on the same transfer.
	Also      []string
	Memo      string
	BlockTime time.Time
}

// New starts a node holding the devnet registry mints.
func New() *Node {
	n := &Node{accounts: map[string][]byte{}, txs: map[string]landed{}, byAddr: map[string][]string{}, height: 1_000}
	n.Mint(DevnetSOLMint, 9)
	n.Mint(DevnetUSDCMint, 6)
	n.Mint(DevnetDUSDMint, 6)
	n.server = httptest.NewServer(http.HandlerFunc(n.serve))
	return n
}

func (n *Node) URL() string { return n.server.URL }
func (n *Node) Close()      { n.server.Close() }

// Mint stores an initialized SPL mint account.
func (n *Node) Mint(address string, decimals byte) {
	data := make([]byte, 82)
	data[44] = decimals
	data[45] = 1
	n.put(address, data)
}

// Plan publishes an active, perpetual subscription plan at its program address.
func (n *Node) Plan(owner solanago.PublicKey, planID uint64, mint string, amount, periodHours uint64) (solanago.PublicKey, error) {
	address, bump, err := subscriptions.DerivePlanPDA(owner, planID)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	mintKey, err := solanago.PublicKeyFromBase58(mint)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	data := make([]byte, subscriptions.PlanAccountSize)
	data[0] = 1 // plan discriminator
	off := 1
	off += copy(data[off:], owner[:])
	data[off], data[off+1] = bump, subscriptions.PlanStatusActive
	off += 2
	binary.LittleEndian.PutUint64(data[off:], planID)
	off += 8
	off += copy(data[off:], mintKey[:])
	binary.LittleEndian.PutUint64(data[off:], amount)
	binary.LittleEndian.PutUint64(data[off+8:], periodHours)
	binary.LittleEndian.PutUint64(data[off+16:], 1_700_000_000)
	n.put(address.String(), data)
	return address, nil
}

// Land records a finalized transfer and returns its signature.
func (n *Node) Land(t Transfer) (string, error) {
	recipient, err := solanago.PublicKeyFromBase58(t.Recipient)
	if err != nil {
		return "", err
	}
	reference, err := solanago.PublicKeyFromBase58(t.Reference)
	if err != nil {
		return "", err
	}
	metas := []*solanago.AccountMeta{solanago.Meta(reference)}
	for _, extra := range t.Also {
		key, err := solanago.PublicKeyFromBase58(extra)
		if err != nil {
			return "", err
		}
		metas = append(metas, solanago.Meta(key))
	}
	var ixs []solanago.Instruction
	if t.Memo != "" {
		ixs = append(ixs, solanaint.NewMemoInstruction(t.Memo))
	}
	native := t.Mint == "" || t.Mint == DevnetSOLMint
	var credited solanago.PublicKey
	if native {
		ix := system.NewTransferInstruction(t.Amount, t.Payer, recipient)
		ix.AccountMetaSlice = append(ix.AccountMetaSlice, metas...)
		ixs = append(ixs, ix.Build())
		credited = recipient
	} else {
		mint, err := solanago.PublicKeyFromBase58(t.Mint)
		if err != nil {
			return "", err
		}
		from, _, _ := solanago.FindAssociatedTokenAddress(t.Payer, mint)
		to, _, _ := solanago.FindAssociatedTokenAddress(recipient, mint)
		ix := token.NewTransferInstruction(t.Amount, from, to, t.Payer, nil)
		ix.Accounts = append(ix.Accounts, metas...)
		ixs = append(ixs, ix.Build())
		credited = to
	}
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(t.Payer))
	if err != nil {
		return "", err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	keys := len(tx.Message.AccountKeys)
	pre, post := make([]uint64, keys), make([]uint64, keys)
	meta := map[string]any{"err": nil, "fee": 5000, "preBalances": pre, "postBalances": post, "preTokenBalances": []any{}, "postTokenBalances": []any{}}
	idx := -1
	for i, k := range tx.Message.AccountKeys {
		if k.Equals(credited) {
			idx = i
		}
	}
	if native {
		pre[0], post[0] = 10_000_000_000, 10_000_000_000-t.Amount
		post[idx] = t.Amount
	} else {
		balance := func(amount uint64) []any {
			return []any{map[string]any{"accountIndex": idx, "mint": t.Mint, "uiTokenAmount": map[string]any{"amount": strconv.FormatUint(amount, 10), "decimals": 6}}}
		}
		meta["preTokenBalances"], meta["postTokenBalances"] = balance(0), balance(t.Amount)
	}
	var sig solanago.Signature
	if _, err := rand.Read(sig[:]); err != nil {
		return "", err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.height++
	n.txs[sig.String()] = landed{raw: raw, meta: meta, blockTime: t.BlockTime, slot: n.height}
	for _, ref := range append([]string{t.Reference}, t.Also...) {
		n.byAddr[ref] = append(n.byAddr[ref], sig.String())
	}
	return sig.String(), nil
}

// AdvanceBlocks moves the chain's block height, expiring blockhashes.
func (n *Node) AdvanceBlocks(blocks uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.height += blocks
}

func (n *Node) put(address string, data []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts[address] = data
}

// signatures answers getSignaturesForAddress newest first, honouring the
// limit and before cursor.
func (n *Node) signatures(params []json.RawMessage) []any {
	var address, before string
	var pageSize int
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &address)
	}
	if len(params) > 1 {
		opts := map[string]json.RawMessage{}
		_ = json.Unmarshal(params[1], &opts)
		_ = json.Unmarshal(opts["limit"], &pageSize)
		_ = json.Unmarshal(opts["before"], &before)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	sigs := n.byAddr[address]
	out := []any{}
	skipping := before != ""
	for i := len(sigs) - 1; i >= 0; i-- {
		if skipping {
			skipping = sigs[i] != before
			continue
		}
		if pageSize > 0 && len(out) == pageSize {
			break
		}
		tx := n.txs[sigs[i]]
		out = append(out, map[string]any{"signature": sigs[i], "slot": tx.slot, "err": nil, "memo": nil, "blockTime": tx.blockTime.Unix(), "confirmationStatus": "finalized"})
	}
	return out
}

func (n *Node) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "getAccountInfo":
		var address string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &address)
		}
		n.mu.Lock()
		data, ok := n.accounts[address]
		n.mu.Unlock()
		var value any
		if ok {
			value = map[string]any{"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false, "lamports": 1_000_000, "owner": solanago.TokenProgramID.String(), "rentEpoch": 0, "space": len(data)}
		}
		reply["result"] = map[string]any{"context": map[string]any{"slot": 1}, "value": value}
	case "getSignaturesForAddress":
		reply["result"] = n.signatures(req.Params)
	case "getSignatureStatuses":
		var sigs []string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &sigs)
		}
		n.mu.Lock()
		values := make([]any, len(sigs))
		for i, sig := range sigs {
			if tx, ok := n.txs[sig]; ok {
				values[i] = map[string]any{"slot": tx.slot, "confirmations": nil, "confirmationStatus": "finalized", "err": nil}
			}
		}
		n.mu.Unlock()
		reply["result"] = map[string]any{"context": map[string]any{"slot": 1}, "value": values}
	case "getTransaction":
		var sig string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &sig)
		}
		n.mu.Lock()
		tx, ok := n.txs[sig]
		n.mu.Unlock()
		if ok {
			reply["result"] = map[string]any{"slot": tx.slot, "blockTime": tx.blockTime.Unix(), "transaction": []string{base64.StdEncoding.EncodeToString(tx.raw), "base64"}, "meta": tx.meta}
		} else {
			reply["result"] = nil
		}
	case "getBlockHeight":
		n.mu.Lock()
		reply["result"] = n.height
		n.mu.Unlock()
	case "getLatestBlockhash":
		var hash solanago.Hash
		_, _ = rand.Read(hash[:])
		n.mu.Lock()
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.height}, "value": map[string]any{"blockhash": hash.String(), "lastValidBlockHeight": n.height + 150}}
		n.mu.Unlock()
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "method not available on the loopback node: " + req.Method}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
