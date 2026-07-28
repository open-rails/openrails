package ccbill

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

type RESTClient struct {
	config *config.CCBillConfig
}

func NewRESTClient(cfg *config.CCBillConfig) *RESTClient {
	return &RESTClient{
		config: requireConfig(cfg),
	}
}

// ValidateWebhookAuth pins an inbound callback to the ctx merchant's armed
// CCBill account. Source-IP authentication lives in webhookauth.CCBillIPAllowed
// at ingress; this is the per-merchant half.
func (c *RESTClient) ValidateWebhookAuth(clientAccnum, clientSubacc string) error {
	if clientAccnum != c.config.ClientAccNum {
		return fmt.Errorf("webhook clientAccnum mismatch: got %s, expected %s", clientAccnum, c.config.ClientAccNum)
	}

	if clientSubacc != c.config.ClientSubAcc {
		return fmt.Errorf("webhook clientSubacc mismatch: got %s, expected %s", clientSubacc, c.config.ClientSubAcc)
	}

	return nil
}
