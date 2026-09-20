package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// HyperSwitchConfig identifies the one trusted custody deployment and its
// vendor-owned browser assets. Merchant settings cannot change either target.
// The SDK is a separate server in the vendor product, hence its explicit URL.
type HyperSwitchConfig struct {
	APIBaseURL string `koanf:"api_base_url"`
	SDKURL     string `koanf:"sdk_url"`
}

func validateHyperSwitch(cfg *Config) error {
	if cfg == nil || cfg.HyperSwitch == nil {
		return nil
	}
	for key, raw := range map[string]string{"api_base_url": cfg.HyperSwitch.APIBaseURL, "sdk_url": cfg.HyperSwitch.SDKURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("hyperswitch.%s must be an absolute HTTPS URL without credentials, query or fragment", key)
		}
		if u.Scheme == "http" {
			ip := net.ParseIP(u.Hostname())
			if !cfg.IsDev() || cfg.TestMode != CredentialPostureSandbox || ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("hyperswitch.%s requires HTTPS outside a development sandbox literal loopback fixture", key)
			}
		}
	}
	return nil
}
