package money

import (
	"context"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPendingInvoiceItemRequiresBusinessTimestamp(t *testing.T) {
	err := insertPendingInvoiceItemTx(context.Background(), nil, uuid.New(), uuid.New(), "USD", "usage", "request-1", 100, time.Time{}, nil)
	require.EqualError(t, err, "invoice item timestamp required")
}
