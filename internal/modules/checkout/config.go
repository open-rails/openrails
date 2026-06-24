package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

func requireCCBillRailConfig(rails config.RailSet) (*config.RailConfig, error) {
	if rails == nil {
		return nil, fmt.Errorf("config is required")
	}
	ccbillProc := rails.GetCCBillRail()
	if ccbillProc == nil {
		return nil, fmt.Errorf("ccbill rail is not configured")
	}
	return ccbillProc, nil
}
