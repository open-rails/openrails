// Package solanafake is a loopback Solana JSON-RPC node for sandbox tests
// (provider_sandbox.solana_rpc_url). It serves declared mints, token accounts,
// plans and subscription authorities, and lands signed transactions as the
// chain would: signatures must verify, token moves apply all-or-nothing, v0
// transactions need opting in, and each commitment sees only what reached it.
package solanafake

import (
	"crypto/rand"
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
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// Devnet mints OpenRails' token registry pins.
const (
	DevnetSOLMint   = "So11111111111111111111111111111111111111112"
	DevnetUSDCMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	DevnetDUSDMint  = "7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T"
	DevnetPYUSDMint = "CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM" // Token-2022
)

type tokenAccount struct {
	owner, mint solanago.PublicKey
	amount      uint64
}

type Node struct {
	server   *httptest.Server
	mu       sync.Mutex
	accounts map[string]account
	tokens   map[string]*tokenAccount
	// landed transactions by signature, and each address's signatures in
	// landing order.
	txs    map[string]*landed
	byAddr map[string][]string
	height uint64
	// token2022 holds each Token-2022 mint's extensions.
	token2022 map[string]*extensions
	// forgotten signatures this node answers as unknown, for so many reads.
	forgotten map[string]int
	// gate stalls every answer until it closes (Hold); stalled names each
	// stalled method.
	gate    chan struct{}
	stalled chan string
}

type extensions struct {
	decimals byte
	fee      *solanaint.TransferFee
	hook     solanago.PublicKey
}

type account struct {
	owner solanago.PublicKey
	data  []byte
}

type landed struct {
	raw       []byte
	v0        bool
	failure   any // the on-chain error; nil when it succeeded
	finalized bool
	meta      map[string]any
	blockTime time.Time
	slot      uint64
}

// Transfer is one synthesized payment a test lands with Pay.
type Transfer struct {
	Payer     solanago.PublicKey
	Recipient string // wallet
	Mint      string // SPL or Token-2022 mint; empty or the SOL mint pays lamports
	Amount    uint64
	Reference string
	// Also names further references on the same transaction.
	Also      []string
	Memo      string
	BlockTime time.Time

	// Split pays the amount in this many transfer instructions.
	Split int
	// Inner moves the money inside another program's call: no top-level
	// transfer instruction names the recipient, only the balances change.
	Inner bool
	// Account is the token account credited instead of the recipient's
	// associated one; it is owned by the recipient.
	Account solanago.PublicKey
	// V0 lands a versioned transaction.
	V0 bool
	// Failed lands the transaction with an on-chain error; nothing moves.
	Failed bool
	// Confirmed leaves the transaction confirmed until Finalize.
	Confirmed bool
	// Garbled serves token balances no reader can parse.
	Garbled bool
}

// New starts a node holding the devnet registry mints.
func New() *Node {
	n := &Node{accounts: map[string]account{}, tokens: map[string]*tokenAccount{}, txs: map[string]*landed{}, byAddr: map[string][]string{}, height: 1_000,
		token2022: map[string]*extensions{}, forgotten: map[string]int{}}
	n.Mint(DevnetSOLMint, 9)
	n.Mint(DevnetUSDCMint, 6)
	n.Mint(DevnetDUSDMint, 6)
	n.Mint2022(DevnetPYUSDMint, 6)
	n.server = httptest.NewServer(http.HandlerFunc(n.serve))
	return n
}

func (n *Node) URL() string { return n.server.URL }

// Hold stalls every answer until release, as a node that stops responding;
// stalled names each stalled method. A caller that gives up is let go.
func (n *Node) Hold() (stalled <-chan string, release func()) {
	gate, names := make(chan struct{}), make(chan string, 64)
	n.mu.Lock()
	n.gate, n.stalled = gate, names
	n.mu.Unlock()
	var once sync.Once
	return names, func() {
		once.Do(func() {
			n.mu.Lock()
			n.gate, n.stalled = nil, nil
			n.mu.Unlock()
			close(gate)
		})
	}
}
func (n *Node) Close() { n.server.Close() }

// Mint stores an initialized SPL Token mint account.
func (n *Node) Mint(address string, decimals byte) {
	n.put(address, solanago.TokenProgramID, mintData(decimals))
}

// Mint2022 stores an initialized Token-2022 mint account.
func (n *Node) Mint2022(address string, decimals byte) {
	n.mu.Lock()
	n.token2022[address] = &extensions{decimals: decimals}
	n.mu.Unlock()
	n.writeMint2022(address)
}

// SetTransferFee sets a Token-2022 mint's transfer fee from now on.
func (n *Node) SetTransferFee(mint string, basisPoints uint16, maximumFee uint64) {
	n.mu.Lock()
	n.token2022[mint].fee = &solanaint.TransferFee{MaximumFee: maximumFee, BasisPts: basisPoints}
	n.mu.Unlock()
	n.writeMint2022(mint)
}

// SetTransferHook sets a Token-2022 mint's transfer hook program.
func (n *Node) SetTransferHook(mint string, program solanago.PublicKey) {
	n.mu.Lock()
	n.token2022[mint].hook = program
	n.mu.Unlock()
	n.writeMint2022(mint)
}

// Forget makes the node answer sig as unknown for the next reads that name
// it, as a lagging or pruned node does.
func (n *Node) Forget(sig string, reads int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.forgotten[sig] = reads
}

// forgets consumes one forgotten read of sig; n.mu held.
func (n *Node) forgets(sig string) bool {
	if n.forgotten[sig] > 0 {
		n.forgotten[sig]--
		return true
	}
	return false
}

func (n *Node) writeMint2022(address string) {
	n.mu.Lock()
	ext := *n.token2022[address]
	n.mu.Unlock()
	data := append(mintData(ext.decimals), make([]byte, 165-82)...)
	data = append(data, 1) // account type: mint
	const feeLen, hookLen uint16 = 108, 64
	tlv := func(typ, size uint16, value []byte) {
		head := make([]byte, 4)
		binary.LittleEndian.PutUint16(head, typ)
		binary.LittleEndian.PutUint16(head[2:], size)
		data = append(append(data, head...), value[:size]...)
	}
	if ext.fee != nil {
		v := make([]byte, feeLen)
		for _, off := range []int{72, 90} { // older and newer fee: the same, in force from epoch 0
			binary.LittleEndian.PutUint64(v[off+8:], ext.fee.MaximumFee)
			binary.LittleEndian.PutUint16(v[off+16:], ext.fee.BasisPts)
		}
		tlv(1, feeLen, v)
	}
	if !ext.hook.IsZero() {
		tlv(14, hookLen, append(make([]byte, 32), ext.hook[:]...))
	}
	n.put(address, solanaint.Token2022ProgramID, data)
}

func mintData(decimals byte) []byte {
	data := make([]byte, 82)
	data[44] = decimals
	data[45] = 1
	return data
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
	n.put(address.String(), subscriptions.ProgramID, data)
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
	n.put(address.String(), subscriptions.ProgramID, data)
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
	var failure any
	var created []string
	for i, inst := range tx.Message.Instructions {
		authority, err := n.apply(tx, inst)
		if authority != "" {
			created = append(created, authority)
		}
		if err != nil {
			failure = map[string]any{"InstructionError": []any{i, map[string]any{"Custom": 1}}}
			for addr, amount := range saved {
				n.tokens[addr].amount = amount
			}
			for _, addr := range created {
				delete(n.accounts, addr)
			}
			break
		}
	}
	n.height++
	n.txs[sig.String()] = &landed{
		slot: n.height, blockTime: blockTime, raw: raw, v0: tx.Message.IsVersioned(), failure: failure, finalized: true,
		meta: map[string]any{
			"err": failure, "preBalances": []any{}, "postBalances": []any{},
			"preTokenBalances": pre, "postTokenBalances": n.tokenBalances(tx),
			"innerInstructions": []any{}, "logMessages": []any{},
			"loadedAddresses": map[string]any{"writable": []any{}, "readonly": []any{}},
		},
	}
	for _, key := range tx.Message.AccountKeys {
		n.byAddr[key.String()] = append(n.byAddr[key.String()], sig.String())
	}
	return sig, nil
}

// apply executes one instruction's effect, returning the address of a
// subscription authority it created; n.mu held.
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
		binary.LittleEndian.PutUint64(authority[98:], n.height)
		address := accounts[1].PublicKey.String()
		n.accounts[address] = account{owner: subscriptions.ProgramID, data: authority}
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

// tokenBalances is the transaction's token balances as meta reports them;
// n.mu held.
func (n *Node) tokenBalances(tx *solanago.Transaction) []any {
	out := []any{}
	for i, key := range tx.Message.AccountKeys {
		acct := n.tokens[key.String()]
		if acct == nil {
			continue
		}
		decimals, program := 6, solanago.TokenProgramID
		if mint, ok := n.accounts[acct.mint.String()]; ok && len(mint.data) > 44 {
			decimals, program = int(mint.data[44]), mint.owner
		}
		out = append(out, map[string]any{
			"accountIndex": i, "mint": acct.mint.String(), "owner": acct.owner.String(), "programId": program.String(),
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

// Pay lands a synthesized payment and returns its signature.
func (n *Node) Pay(t Transfer) (string, error) {
	recipient, err := solanago.PublicKeyFromBase58(t.Recipient)
	if err != nil {
		return "", err
	}
	refs := []*solanago.AccountMeta{}
	for _, r := range append([]string{t.Reference}, t.Also...) {
		key, err := solanago.PublicKeyFromBase58(r)
		if err != nil {
			return "", err
		}
		refs = append(refs, solanago.Meta(key))
	}
	var ixs []solanago.Instruction
	if t.Memo != "" {
		ixs = append(ixs, solanaint.NewMemoInstruction(t.Memo))
	}
	parts := max(t.Split, 1)
	native := t.Mint == "" || t.Mint == DevnetSOLMint
	var credited solanago.PublicKey
	var mint solanago.PublicKey
	var program solanago.PublicKey
	if native {
		credited = recipient
	} else {
		if mint, err = solanago.PublicKeyFromBase58(t.Mint); err != nil {
			return "", err
		}
		n.mu.Lock()
		program = n.accounts[t.Mint].owner
		n.mu.Unlock()
		if program.IsZero() {
			return "", errors.New("solanafake: unknown mint " + t.Mint)
		}
		credited = t.Account
		if credited.IsZero() {
			if credited, err = solanaint.AssociatedTokenAddress(recipient, mint, program); err != nil {
				return "", err
			}
		}
	}
	if t.Inner {
		// A program call that names the credited account and the references;
		// the transfer happens inside it.
		accounts := append([]*solanago.AccountMeta{solanago.Meta(t.Payer).SIGNER().WRITE(), solanago.Meta(credited).WRITE()}, refs...)
		ixs = append(ixs, solanago.NewInstruction(solanago.NewWallet().PublicKey(), accounts, []byte{1}))
	} else {
		for i := range parts {
			amount := t.Amount / uint64(parts)
			if i == parts-1 {
				amount = t.Amount - amount*uint64(parts-1)
			}
			if native {
				ix := system.NewTransferInstruction(amount, t.Payer, recipient)
				ix.AccountMetaSlice = append(ix.AccountMetaSlice, refs...)
				ixs = append(ixs, ix.Build())
				continue
			}
			from, err := solanaint.AssociatedTokenAddress(t.Payer, mint, program)
			if err != nil {
				return "", err
			}
			built := token.NewTransferCheckedInstruction(amount, 6, from, mint, credited, t.Payer, nil).Build()
			data, err := built.Data()
			if err != nil {
				return "", err
			}
			ixs = append(ixs, solanago.NewInstruction(program, append(built.Accounts(), refs...), data))
		}
	}
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(t.Payer))
	if err != nil {
		return "", err
	}
	if t.V0 {
		if _, err := tx.Message.SetVersion(solanago.MessageVersionV0); err != nil {
			return "", err
		}
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	keys := len(tx.Message.AccountKeys)
	pre, post := make([]uint64, keys), make([]uint64, keys)
	meta := map[string]any{"err": nil, "fee": 5000, "preBalances": pre, "postBalances": post, "preTokenBalances": []any{}, "postTokenBalances": []any{},
		"loadedAddresses": map[string]any{"writable": []any{}, "readonly": []any{}}, "innerInstructions": []any{}, "logMessages": []any{}}
	idx := -1
	for i, k := range tx.Message.AccountKeys {
		if k.Equals(credited) {
			idx = i
		}
	}
	moved := t.Amount
	n.mu.Lock()
	if ext := n.token2022[t.Mint]; ext != nil && ext.fee != nil {
		moved -= ext.fee.For(t.Amount)
	}
	n.mu.Unlock()
	if t.Failed {
		meta["err"] = map[string]any{"InstructionError": []any{len(ixs) - 1, "Custom"}}
		moved = 0
	}
	pre[0], post[0] = 10_000_000_000, 10_000_000_000-5000
	if native {
		post[idx] = moved
	} else {
		balance := func(amount uint64) []any {
			return []any{map[string]any{"accountIndex": idx, "mint": t.Mint, "owner": recipient.String(), "programId": program.String(),
				"uiTokenAmount": map[string]any{"amount": strconv.FormatUint(amount, 10), "decimals": 6}}}
		}
		meta["preTokenBalances"], meta["postTokenBalances"] = balance(0), balance(moved)
	}
	if t.Garbled {
		meta["postTokenBalances"] = []any{map[string]any{"accountIndex": idx, "mint": t.Mint, "owner": recipient.String(),
			"uiTokenAmount": map[string]any{"amount": "not-a-number", "decimals": 6}}}
	}
	var sig solanago.Signature
	if _, err := rand.Read(sig[:]); err != nil {
		return "", err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.height++
	n.txs[sig.String()] = &landed{raw: raw, v0: t.V0, failure: meta["err"], finalized: !t.Confirmed, meta: meta, blockTime: t.BlockTime, slot: n.height}
	for _, ref := range append([]string{t.Reference}, t.Also...) {
		n.byAddr[ref] = append(n.byAddr[ref], sig.String())
	}
	return sig.String(), nil
}

// Finalize moves a confirmed transaction to finalized.
func (n *Node) Finalize(sig string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if tx, ok := n.txs[sig]; ok {
		tx.finalized = true
	}
}

// AdvanceBlocks moves the chain's block height, expiring blockhashes.
func (n *Node) AdvanceBlocks(blocks uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.height += blocks
}

func (n *Node) put(address string, owner solanago.PublicKey, data []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts[address] = account{owner: owner, data: data}
}

// visible reports whether a transaction is served at the commitment a request
// names (finalized when it names none).
func (tx *landed) visible(opts map[string]json.RawMessage) bool {
	var commitment string
	_ = json.Unmarshal(opts["commitment"], &commitment)
	return tx.finalized || commitment == "confirmed" || commitment == "processed"
}

func options(params []json.RawMessage, i int) map[string]json.RawMessage {
	opts := map[string]json.RawMessage{}
	if len(params) > i {
		_ = json.Unmarshal(params[i], &opts)
	}
	return opts
}

// signatures answers getSignaturesForAddress newest first, honouring the
// commitment, limit and before/until cursors.
func (n *Node) signatures(params []json.RawMessage) []any {
	var address, before, until string
	var pageSize int
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &address)
	}
	opts := options(params, 1)
	_ = json.Unmarshal(opts["limit"], &pageSize)
	_ = json.Unmarshal(opts["before"], &before)
	_ = json.Unmarshal(opts["until"], &until)
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []any{}
	if before != "" && n.forgets(before) {
		return out
	}
	sigs := n.byAddr[address]
	skipping := before != ""
	for i := len(sigs) - 1; i >= 0; i-- {
		if until != "" && sigs[i] == until {
			break
		}
		if skipping {
			skipping = sigs[i] != before
			continue
		}
		tx := n.txs[sigs[i]]
		if !tx.visible(opts) {
			continue
		}
		if pageSize > 0 && len(out) == pageSize {
			break
		}
		status := "finalized"
		if !tx.finalized {
			status = "confirmed"
		}
		out = append(out, map[string]any{"signature": sigs[i], "slot": tx.slot, "err": tx.failure, "memo": nil, "blockTime": tx.blockTime.Unix(), "confirmationStatus": status})
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
	n.mu.Lock()
	gate, stalled := n.gate, n.stalled
	n.mu.Unlock()
	if gate != nil {
		select {
		case stalled <- req.Method:
		default:
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "getAccountInfo":
		var address string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &address)
		}
		n.mu.Lock()
		acct, ok := n.accounts[address]
		slot := n.height
		n.mu.Unlock()
		var value any
		if ok {
			value = map[string]any{"data": []string{base64.StdEncoding.EncodeToString(acct.data), "base64"}, "executable": false, "lamports": 1_000_000, "owner": acct.owner.String(), "rentEpoch": 0, "space": len(acct.data)}
		}
		reply["result"] = map[string]any{"context": map[string]any{"slot": slot}, "value": value}
	case "getTokenAccountBalance":
		var address string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &address)
		}
		n.mu.Lock()
		if acct := n.tokens[address]; acct != nil {
			reply["result"] = map[string]any{"context": map[string]any{"slot": n.height}, "value": map[string]any{"amount": strconv.FormatUint(acct.amount, 10), "decimals": 6, "uiAmountString": strconv.FormatUint(acct.amount, 10)}}
		} else {
			reply["error"] = map[string]any{"code": -32602, "message": "Invalid param: could not find account"}
		}
		n.mu.Unlock()
	case "sendTransaction":
		// The server's own submissions (recurring pulls) land at once, as a
		// finalized transaction would; a token move that cannot apply lands failed.
		var encoded string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &encoded)
		}
		if sig, err := n.send(encoded); err != nil {
			reply["error"] = map[string]any{"code": -32002, "message": err.Error()}
		} else {
			reply["result"] = sig.String()
		}
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
			if tx, ok := n.txs[sig]; ok && !n.forgets(sig) {
				status := "finalized"
				if !tx.finalized {
					status = "confirmed"
				}
				values[i] = map[string]any{"slot": tx.slot, "confirmations": nil, "confirmationStatus": status, "err": tx.failure}
			}
		}
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.height}, "value": values}
		n.mu.Unlock()
	case "getTransaction":
		var sig string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &sig)
		}
		opts := options(req.Params, 1)
		n.mu.Lock()
		tx, ok := n.txs[sig]
		n.mu.Unlock()
		switch {
		case !ok || !tx.visible(opts):
			reply["result"] = nil
		case tx.v0 && opts["maxSupportedTransactionVersion"] == nil:
			reply["error"] = map[string]any{"code": -32015, "message": "Transaction version (0) is not supported by the requesting client. Please try the request again with the following configuration parameter: \"maxSupportedTransactionVersion\": 0"}
		default:
			result := map[string]any{"slot": tx.slot, "blockTime": tx.blockTime.Unix(), "transaction": []string{base64.StdEncoding.EncodeToString(tx.raw), "base64"}, "meta": tx.meta}
			if tx.v0 {
				result["version"] = 0
			} else {
				result["version"] = "legacy"
			}
			reply["result"] = result
		}
	case "getEpochInfo":
		n.mu.Lock()
		reply["result"] = map[string]any{"absoluteSlot": n.height, "blockHeight": n.height, "epoch": 5, "slotIndex": 0, "slotsInEpoch": 432_000, "transactionCount": nil}
		n.mu.Unlock()
	case "getBlockHeight":
		n.mu.Lock()
		reply["result"] = n.height
		n.mu.Unlock()
	case "getLatestBlockhash":
		var hash solanago.Hash
		n.mu.Lock()
		binary.LittleEndian.PutUint64(hash[:], n.height)
		reply["result"] = map[string]any{"context": map[string]any{"slot": n.height}, "value": map[string]any{"blockhash": hash.String(), "lastValidBlockHeight": n.height + 150}}
		n.mu.Unlock()
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "method not available on the loopback node: " + req.Method}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
