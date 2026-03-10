package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

func requireCCBillProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	ccbillProc := cfg.GetCCBillProcessor()
	if ccbillProc == nil {
		return nil, fmt.Errorf("ccbill processor is not configured")
	}
	return ccbillProc, nil
}
