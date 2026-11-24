package ccbill

import (
	"context"
	"fmt"
	"time"
)

// ProbeCredentials verifies the DataLink account through a bounded,
// read-only export in test mode, which CCBill's guide exempts from DataLink's
// hourly limit: proving credentials never takes the account's slot, whoever
// names the account. It never creates or changes provider state.
func (c *DataLinkClient) ProbeCredentials(ctx context.Context) error {
	if err := c.ValidateConfig(); err != nil {
		return err
	}
	probe := *c
	probe.DevMode = true // testMode=1: authenticated, not paced
	end := probe.now()
	if _, err := probe.FetchTransactionExport(ctx, end.Add(-time.Minute), end, []DataLinkTxnType{DataLinkTxnRebill}); err != nil {
		return fmt.Errorf("ccbill credential probe: %w", err)
	}
	return nil
}
