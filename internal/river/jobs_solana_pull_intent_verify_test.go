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
	for _, tc := range []struct {
		name  string
		chain pullChain
		want  sigVerdict
	}{
		{"not found is not nonexecution", pullChain{err: rpc.ErrNotFound}, sigVerdictUnknown},
		{"null is not nonexecution", pullChain{}, sigVerdictUnknown},
		{"read failure stays unresolved", pullChain{err: errors.New("RPC unavailable")}, sigVerdictUnknown},
		{"landed payment needs repair", pullChain{result: &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{}}}, sigVerdictLanded},
		{"reverted payment permits retry", pullChain{result: &rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{Err: "reverted"}}}, sigVerdictNotLanded},
		{"missing execution metadata stays unresolved", pullChain{result: &rpc.GetTransactionResult{}}, sigVerdictUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SolanaPullIntentHandler{Chain: &tc.chain}
			_, verdict := h.checkSignature(t.Context(), solanago.Signature{1}.String())
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
	h := &SolanaPullIntentHandler{Chain: solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: server.URL, LoopbackFixture: true})}
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
