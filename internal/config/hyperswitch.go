package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

func validateHyperSwitch(cfg *billing.Config) error {
	if cfg == nil || cfg.HyperSwitch == (billing.HyperSwitchConfig{}) {
		return nil
	}
	for key, raw := range map[string]string{"api_base_url": cfg.HyperSwitch.APIBaseURL, "sdk_url": cfg.HyperSwitch.SDKURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("hyperswitch.%s must be an absolute HTTPS URL without credentials, query or fragment", key)
		}
		if u.Scheme == "http" {
			ip := net.ParseIP(u.Hostname())
			if !cfg.HyperSwitch.AllowLoopbackHTTP || ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("hyperswitch.%s requires HTTPS outside a development sandbox literal loopback fixture", key)
			}
		}
	}
	return nil
}
