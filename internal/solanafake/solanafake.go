// Package solanafake is a loopback Solana JSON-RPC node for sandbox tests. It
// serves the accounts a test declares — SPL mints, token accounts, published
// subscription plans and subscription authorities — to an OpenRails whose
// provider_sandbox.solana_rpc_url points at it, and lands signed transactions
// (the test's, and the server's own sendTransaction) the way the chain would: every signature must verify, and SPL token moves
// (transfer, transfer_checked, the subscriptions program's
// transfer_subscription) and new subscription authorities apply
// all-or-nothing.
package solanafake

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// Devnet mints OpenRails' token registry pins.
const (
	DevnetSOLMint  = "So11111111111111111111111111111111111111112"
	DevnetUSDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	DevnetDUSDMint = "7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T"
)

type tokenAccount struct {
	owner, mint solanago.PublicKey
	amount      uint64
}

type landed struct {
	slot      uint64
	blockTime int64
	raw       []byte
	meta      map[string]any
	failed    any
}

type Node struct {
	server   *httptest.Server
	mu       sync.Mutex
	accounts map[string][]byte
	tokens   map[string]*tokenAccount
	txs      map[string]*landed
	slot     uint64
}

// New starts a node holding the devnet registry mints.
func New() *Node {
	n := &Node{accounts: map[string][]byte{}, tokens: map[string]*tokenAccount{}, txs: map[string]*landed{}, slot: 1}
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

// Authority stores an owner's SubscriptionAuthority for mint, as a returning
// subscriber has one.
func (n *Node) Authority(owner, mint solanago.PublicKey, initID uint64) error {
	address, _, err := subscriptions.DeriveSubscriptionAuthority(owner, mint)
	if err != nil {
		return err
	}
	data := make([]byte, 106)
	binary.LittleEndian.PutUint64(data[98:], initID)
	n.put(address.String(), data)
	return nil
}

// Fund sets the balance of owner's associated token account for mint.
func (n *Node) Fund(owner, mint solanago.PublicKey, amount uint64) solanago.PublicKey {
	ata, _, err := solanago.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		panic(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tokens[ata.String()] = &tokenAccount{owner: owner, mint: mint, amount: amount}
	return ata
}

// Balance is owner's associated token account balance for mint.
func (n *Node) Balance(owner, mint solanago.PublicKey) uint64 {
	ata, _, _ := solanago.FindAssociatedTokenAddress(owner, mint)
	n.mu.Lock()
	defer n.mu.Unlock()
	if acct := n.tokens[ata.String()]; acct != nil {
		return acct.amount
	}
	return 0
}

// Land executes a signed transaction at blockTime and returns its signature.
// Like the chain it refuses a transaction whose signatures do not verify or
// that already landed; a token move that cannot apply lands the transaction as
// failed with no effect.
func (n *Node) Land(tx *solanago.Transaction, blockTime time.Time) (solanago.Signature, error) {
	if err := tx.VerifySignatures(); err != nil {
		return solanago.Signature{}, fmt.Errorf("solanafake: refused: %w", err)
	}
	sig := tx.Signatures[0]
	raw, err := tx.MarshalBinary()
	if err != nil {
		return solanago.Signature{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, dup := n.txs[sig.String()]; dup {
		return solanago.Signature{}, errors.New("solanafake: refused: already processed")
	}
	pre := n.tokenBalances(tx)
	saved := map[string]uint64{}
	for addr, acct := range n.tokens {
		saved[addr] = acct.amount
	}
	var failed any
	var created []string
	for i, inst := range tx.Message.Instructions {
		authority, err := n.apply(tx, inst)
		if authority != "" {
			created = append(created, authority)
		}
		if err != nil {
			failed = map[string]any{"InstructionError": []any{i, map[string]any{"Custom": 1}}}
			for addr, amount := range saved {
				n.tokens[addr].amount = amount
			}
			for _, addr := range created {
				delete(n.accounts, addr)
			}
			break
		}
	}
	n.slot++
	n.txs[sig.String()] = &landed{
		slot: n.slot, blockTime: blockTime.Unix(), raw: raw, failed: failed,
		meta: map[string]any{
			"err": failed, "preBalances": []any{}, "postBalances": []any{},
			"preTokenBalances": pre, "postTokenBalances": n.tokenBalances(tx),
			"innerInstructions": []any{}, "logMessages": []any{},
			"loadedAddresses": map[string]any{"writable": []any{}, "readonly": []any{}},
		},
	}
	return sig, nil
}

// apply executes one instruction's effect, returning the address of a
// subscription authority it created.
func (n *Node) apply(tx *solanago.Transaction, inst solanago.CompiledInstruction) (string, error) {
	program, err := tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
	if err != nil {
		return "", err
	}
	accounts, err := inst.ResolveInstructionAccounts(&tx.Message)
	if err != nil {
		return "", err
	}
	data := inst.Data
	switch {
	case program.Equals(solanago.TokenProgramID) && len(data) >= 9 && data[0] == 3 && len(accounts) >= 2: // Transfer
		return "", n.move(accounts[0].PublicKey, accounts[1].PublicKey, binary.LittleEndian.Uint64(data[1:9]))
	case program.Equals(solanago.TokenProgramID) && len(data) >= 9 && data[0] == 12 && len(accounts) >= 3: // TransferChecked
		return "", n.move(accounts[0].PublicKey, accounts[2].PublicKey, binary.LittleEndian.Uint64(data[1:9]))
	case program.Equals(subscriptions.ProgramID) && len(data) >= 9 && data[0] == 10 && len(accounts) >= 5: // transfer_subscription
		return "", n.move(accounts[3].PublicKey, accounts[4].PublicKey, binary.LittleEndian.Uint64(data[1:9]))
	case program.Equals(subscriptions.ProgramID) && len(data) == 1 && data[0] == 0 && len(accounts) >= 2: // initialize_subscription_authority
		authority := make([]byte, 106)
		binary.LittleEndian.PutUint64(authority[98:], n.slot)
		address := accounts[1].PublicKey.String()
		n.accounts[address] = authority
		return address, nil
	}
	return "", nil
}

func (n *Node) move(from, to solanago.PublicKey, amount uint64) error {
	src, dst := n.tokens[from.String()], n.tokens[to.String()]
	if src == nil || dst == nil || !src.mint.Equals(dst.mint) || src.amount < amount {
		return errors.New("token move refused")
	}
	src.amount -= amount
	dst.amount += amount
	return nil
}

func (n *Node) tokenBalances(tx *solanago.Transaction) []any {
	out := []any{}
	for i, key := range tx.Message.AccountKeys {
		acct := n.tokens[key.String()]
		if acct == nil {
			continue
		}
		decimals := 6
		if mint := n.accounts[acct.mint.String()]; len(mint) > 44 {
			decimals = int(mint[44])
		}
		out = append(out, map[string]any{
			"accountIndex": i, "mint": acct.mint.String(), "owner": acct.owner.String(), "programId": solanago.TokenProgramID.String(),
			"uiTokenAmount": map[string]any{"amount": strconv.FormatUint(acct.amount, 10), "decimals": decimals, "uiAmountString": strconv.FormatUint(acct.amount, 10)},
		})
	}
	return out
}

func (n *Node) send(encoded string) (solanago.Signature, error) {
	tx, err := solanago.TransactionFromBase64(encoded)
	if err != nil {
		return solanago.Signature{}, fmt.Errorf("solanafake: decode transaction: %w", err)
	}
	return n.Land(tx, time.Now())
}

func (n *Node) put(address string, data []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts[address] = data
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
	var first string
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params[0], &first)
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if req.Method == "sendTransaction" {
		// The server's own submissions (recurring pulls) land at once, as a
		// confirmed transaction would; a token move that cannot apply lands failed.
		sig, err := n.send(first)
		if err != nil {
			reply["error"] = map[string]any{"code": -32002, "message": err.Error()}
		} else {
			reply["result"] = sig.String()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
		return
	}
	n.mu.Lock()
	switch req.Method {
	case "getAccountInfo":
		var value any
		if data, ok := n.accounts[first]; ok {
			value = map[string]any{"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false, "lamports": 1_000_000, "owner": solanago.TokenProgramID.String(), "rentEpoch": 0, "space": len(data)}
		}
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.slot}, "value": value}
	case "getTokenAccountBalance":
		if acct := n.tokens[first]; acct != nil {
			reply["result"] = map[string]any{"context": map[string]any{"slot": n.slot}, "value": map[string]any{"amount": strconv.FormatUint(acct.amount, 10), "decimals": 6, "uiAmountString": strconv.FormatUint(acct.amount, 10)}}
		} else {
			reply["error"] = map[string]any{"code": -32602, "message": "Invalid param: could not find account"}
		}
	case "getLatestBlockhash":
		var hash solanago.Hash
		binary.LittleEndian.PutUint64(hash[:], n.slot)
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.slot}, "value": map[string]any{"blockhash": hash.String(), "lastValidBlockHeight": n.slot + 150}}
	case "getBlockHeight":
		reply["result"] = n.slot
	case "getSignatureStatuses":
		var sigs []string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &sigs)
		}
		statuses := make([]any, len(sigs))
		for i, s := range sigs {
			if tx := n.txs[s]; tx != nil {
				statuses[i] = map[string]any{"slot": tx.slot, "confirmations": nil, "err": tx.failed, "confirmationStatus": "finalized"}
			}
		}
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.slot}, "value": statuses}
	case "getTransaction":
		var result any
		if tx := n.txs[first]; tx != nil {
			result = map[string]any{"slot": tx.slot, "blockTime": tx.blockTime, "meta": tx.meta, "transaction": []string{base64.StdEncoding.EncodeToString(tx.raw), "base64"}}
		}
		reply["result"] = result
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "method not available on the loopback node: " + req.Method}
	}
	n.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
