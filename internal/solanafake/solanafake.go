// Package solanafake is a loopback Solana JSON-RPC node for sandbox tests. It
// serves the accounts a test declares — SPL mints and published subscription
// plans — to an OpenRails whose provider_sandbox.solana_rpc_url points at it.
package solanafake

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	solanago "github.com/gagliardetto/solana-go"

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
}

// New starts a node holding the devnet registry mints.
func New() *Node {
	n := &Node{accounts: map[string][]byte{}}
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
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "method not available on the loopback node: " + req.Method}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
