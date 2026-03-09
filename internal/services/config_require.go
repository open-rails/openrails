package services

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
)

func requireCCBillProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("CCBill configuration is not available")
	}
	proc := cfg.GetCCBillProcessor()
	if proc == nil {
		return nil, fmt.Errorf("CCBill configuration is not available")
	}
	return proc, nil
}

func requireStripeProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("stripe configuration is not available")
	}
	proc := cfg.GetStripeProcessor()
	if proc == nil {
		return nil, fmt.Errorf("stripe configuration is not available")
	}
	return proc, nil
}

func requireStripeSecretKey(cfg *config.Config) (*config.ProcessorConfig, string, error) {
	proc, err := requireStripeProcessorConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	secretKey := strings.TrimSpace(proc.SecretKey)
	if secretKey == "" {
		return nil, "", fmt.Errorf("stripe secret key is not configured")
	}
	return proc, secretKey, nil
}

func requireSolanaProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	proc := cfg.GetSolanaProcessor()
	if proc == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	return proc, nil
}
