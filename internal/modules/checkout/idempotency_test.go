package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingIdempotencyCompleter struct {
	operation string
	key       string
	result    json.RawMessage
	err       error
}

func (f *failingIdempotencyCompleter) Complete(_ context.Context, operation, key string, result json.RawMessage) error {
	f.operation = operation
	f.key = key
	f.result = result
	return f.err
}

func TestCompleteCheckoutIdempotencyLogsSafeContext(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)

	const (
		operation     = "checkout_session"
		key           = "idem-test-key"
		secretMarker  = "sensitive-result-must-not-be-logged"
		failureReason = "redis unavailable"
	)
	result := json.RawMessage(`{"provider_payload":"` + secretMarker + `"}`)
	store := &failingIdempotencyCompleter{err: errors.New(failureReason)}

	completeCheckoutIdempotency(context.Background(), store, operation, key, result)

	assert.Equal(t, operation, store.operation)
	assert.Equal(t, key, store.key)
	assert.Equal(t, result, store.result)

	var completionLog *log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Message == "checkout idempotency completion failed" {
			completionLog = entry
			break
		}
	}
	require.NotNil(t, completionLog)
	assert.Equal(t, log.ErrorLevel, completionLog.Level)
	assert.Equal(t, operation, completionLog.Data["operation"])
	assert.Equal(t, key, completionLog.Data["idempotency_key"])
	assert.ErrorContains(t, completionLog.Data[log.ErrorKey].(error), failureReason)

	logged := completionLog.Message + fmt.Sprint(completionLog.Data)
	assert.False(t, strings.Contains(logged, secretMarker), "idempotency result payload must not be logged")
}
