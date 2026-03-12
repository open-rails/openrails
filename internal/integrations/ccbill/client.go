package ccbill

import (
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

type RESTClient struct {
	config *config.CCBillConfig
}

func NewRESTClient(cfg *config.CCBillConfig) *RESTClient {
	return &RESTClient{
		config: requireConfig(cfg),
	}
}

func (c *RESTClient) ValidateWebhookIP(clientIP string) error {
	if iputil.IsValidCCBillIP(clientIP) {
		return nil
	}

	return fmt.Errorf("webhook request from unauthorized IP: %s", clientIP)
}

func (c *RESTClient) ValidateWebhookAuth(clientAccnum, clientSubacc string) error {
	if clientAccnum != c.config.ClientAccNum {
		return fmt.Errorf("webhook clientAccnum mismatch: got %s, expected %s", clientAccnum, c.config.ClientAccNum)
	}

	if clientSubacc != c.config.ClientSubAcc {
		return fmt.Errorf("webhook clientSubacc mismatch: got %s, expected %s", clientSubacc, c.config.ClientSubAcc)
	}

	return nil
}
