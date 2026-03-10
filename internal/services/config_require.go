package services

import (
	"fmt"

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
