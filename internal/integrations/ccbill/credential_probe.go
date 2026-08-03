package ccbill

import (
	"context"
	"fmt"
	"time"
)

// ProbeCredentials verifies the DataLink account through a bounded, read-only
// export window. It never creates or changes provider state.
func (c *DataLinkClient) ProbeCredentials(ctx context.Context) error {
	if err := c.ValidateConfig(); err != nil {
		return err
	}
	end := time.Now().UTC()
	if _, err := c.FetchTransactionExport(ctx, end.Add(-time.Second), end, []DataLinkTxnType{DataLinkTxnRebill}); err != nil {
		return fmt.Errorf("ccbill credential probe: %w", err)
	}
	return nil
}
