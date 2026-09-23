package ccbill

import (
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

// requireArmed refuses every CCBill mutation under sandbox posture. CCBill
// exposes no authoritative account test-mode signal (DataLink testMode=1 only
// selects synthetic report data), so a sandbox deployment can never prove a
// credential cannot move real money. Only a declared loopback fixture passes.
func (c *DataLinkClient) requireArmed() error {
	if !c.DevMode {
		return nil
	}
	if !c.LoopbackFixture {
		return fmt.Errorf("%w: ccbill %s-%s: %s", providerposture.ErrDisarmed, c.ClientAccNum, c.ClientSubAcc, providerposture.Unsupported)
	}
	if err := config.ValidateLoopbackGatewayURL(c.BaseURL); err != nil {
		return fmt.Errorf("%w: loopback fixture: %w", providerposture.ErrDisarmed, err)
	}
	return nil
}
