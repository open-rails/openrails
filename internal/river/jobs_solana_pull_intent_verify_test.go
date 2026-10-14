package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
)

type pullChain struct {
	result *rpc.GetTransactionResult
	err    error
}

func (c *pullChain) GetTransaction(context.Context, solanago.Signature) (*rpc.GetTransactionResult, error) {
	return c.result, c.err
}

func TestPullRetryNeedsNonexecutionEvidence(t *testing.T) {
	wallet := solanago.NewWallet()
	tx, err := solanago.NewTransaction([]solanago.Instruction{solanaint.NewMemoInstruction(solanaint.PurchaseMemo(uuid.New()))}, solanago.Hash{1}, solanago.TransactionPayer(wallet.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solanago.PublicKey) *solanago.PrivateKey { return &wallet.PrivateKey })
	require.NoError(t, err)
	envelope := func(tx *solanago.Transaction, failure any) *rpc.GetTransactionResult {
		t.Helper()
		encoded, err := tx.ToBase64()
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{"transaction": []string{encoded, "base64"}, "meta": map[string]any{"err": failure}})
		require.NoError(t, err)
		var result rpc.GetTransactionResult
		require.NoError(t, json.Unmarshal(body, &result))
		return &result
	}
	bad := *tx
	bad.Message.RecentBlockhash[0]++
	other := bad
	other.Signatures = nil
	_, err = other.Sign(func(key solanago.PublicKey) *solanago.PrivateKey { return &wallet.PrivateKey })
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		chain pullChain
		want  sigVerdict
	}{
		{"not found is not nonexecution", pullChain{err: rpc.ErrNotFound}, sigVerdictUnknown},
		{"null is not nonexecution", pullChain{}, sigVerdictUnknown},
		{"read failure stays unresolved", pullChain{err: errors.New("RPC unavailable")}, sigVerdictUnknown},
		{"landed payment needs repair", pullChain{result: envelope(tx, nil)}, sigVerdictLanded},
		{"reverted payment permits retry", pullChain{result: envelope(tx, "reverted")}, sigVerdictNotLanded},
		{"metadata alone is not nonexecution", pullChain{result: &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{Err: "reverted"}}}, sigVerdictUnknown},
		{"another signed transaction proves nothing", pullChain{result: envelope(&other, "reverted")}, sigVerdictUnknown},
		{"invalid required signature proves nothing", pullChain{result: envelope(&bad, "reverted")}, sigVerdictUnknown},
		{"missing execution metadata stays unresolved", pullChain{result: &rpc.GetTransactionResult{}}, sigVerdictUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SolanaPullIntentHandler{Chain: &tc.chain}
			_, verdict := h.checkSignature(t.Context(), tx.Signatures[0].String())
			require.Equal(t, tc.want, verdict)
		})
	}
}

func TestVerifyPullMissingHistoryStaysUnresolved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Expiry and an old retention boundary cannot establish that a null
		// historical transaction lookup really means no payment occurred.
		result := "null"
		switch req.Method {
		case "getTransaction":
		case "getEpochInfo":
			result = `{"absoluteSlot":200,"blockHeight":101}`
		case "getSignatureStatuses":
			result = `{"context":{"slot":200},"value":[null]}`
		case "getFirstAvailableBlock":
			result = "1"
		default:
			t.Errorf("unexpected RPC method %s", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer server.Close()
	h := &SolanaPullIntentHandler{Chain: solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: server.URL, Network: "devnet"})}
	payload, err := json.Marshal(SolanaPullPayload{SubscriptionPDA: "subscription", RowID: uuid.New(), NextPullAt: time.Now()})
	require.NoError(t, err)
	intent := gen.BillingProviderIntent{Payload: payload, ResultEvidence: []byte(fmt.Sprintf(
		`{"transaction_id":%q,"last_valid_block_height":100,"blockhash_slot":50}`, solanago.Signature{1}.String()))}
	require.Equal(t, intents.OutcomeAmbiguous, h.Verify(t.Context(), intent).Class)

	for _, tc := range []struct {
		evidence string
		want     intents.OutcomeClass
	}{
		{`{"transaction_id":123}`, intents.OutcomeParked},
		{`{"transaction_id":"invalid"}`, intents.OutcomeAmbiguous},
		{`{}`, intents.OutcomeRetryable},
	} {
		intent.ResultEvidence = []byte(tc.evidence)
		require.Equal(t, tc.want, h.Verify(t.Context(), intent).Class, tc.evidence)
	}
}
