package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

func requireCCBillProcessorConfig(processors config.ProcessorSet) (*config.ProcessorConfig, error) {
	if processors == nil {
		return nil, fmt.Errorf("config is required")
	}
	ccbillProc := processors.GetCCBillProcessor()
	if ccbillProc == nil {
		return nil, fmt.Errorf("ccbill processor is not configured")
	}
	return ccbillProc, nil
}
