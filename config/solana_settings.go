package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Solana rail-account `settings` keys (#711). These are the per-merchant home
// of the Solana runtime knobs: standalone operators declare them in the
// merchant config manifest under merchants.<slug>.accounts.<key>.solana.settings,
// and they are stored on the rail_merchant_accounts row (same mechanism as the
// NMI tokenization_key). Store settings win over the embedded boot plane (#699).
const (
	SolanaSettingRPCProvider = "rpc_provider"
	SolanaSettingRPCAPIKey   = "rpc_api_key"
	SolanaSettingTokens      = "tokens"
	// SolanaSettingRecipientWallet is consumed by the checkout money path (not
	// parsed here); listed so strict manifest validation accepts it.
	SolanaSettingRecipientWallet = "recipient_wallet"
)

// solanaKnownSettingKeys is the strict allowlist for manifest validation: a
// typo'd key fails the push instead of silently arming nothing.
var solanaKnownSettingKeys = map[string]bool{
	SolanaSettingRPCProvider:     true,
	SolanaSettingRPCAPIKey:       true,
	SolanaSettingTokens:          true,
	SolanaSettingRecipientWallet: true,
}

// SolanaAccountSettings is the typed view of a Solana rail-account settings
// map. Nil/absent fields mean "not declared" (the boot plane or defaults apply).
type SolanaAccountSettings struct {
	RPCProvider string
	RPCAPIKey   string
	Tokens      map[string]TokenConfig
}

// IsZero reports whether no known setting was declared.
func (s SolanaAccountSettings) IsZero() bool {
	return s.RPCProvider == "" && s.RPCAPIKey == "" && len(s.Tokens) == 0
}

// ParseSolanaAccountSettings parses the known Solana runtime knobs out of a
// rail-account settings map. Unknown keys are ignored (forward-compat for
// stored rows); malformed values for KNOWN keys error loudly — a typo'd value
// must never silently pick a behavior.
func ParseSolanaAccountSettings(settings map[string]any) (SolanaAccountSettings, error) {
	var out SolanaAccountSettings
	if len(settings) == 0 {
		return out, nil
	}
	if raw, ok := settings[SolanaSettingRPCProvider]; ok {
		v, err := settingString(SolanaSettingRPCProvider, raw)
		if err != nil {
			return out, err
		}
		out.RPCProvider = strings.ToLower(v)
	}
	if raw, ok := settings[SolanaSettingRPCAPIKey]; ok {
		v, err := settingString(SolanaSettingRPCAPIKey, raw)
		if err != nil {
			return out, err
		}
		out.RPCAPIKey = v
	}
	if raw, ok := settings[SolanaSettingTokens]; ok {
		tokens, err := settingTokens(raw)
		if err != nil {
			return out, err
		}
		out.Tokens = tokens
	}
	switch out.RPCProvider {
	case "", "helius", "public":
	default:
		return out, fmt.Errorf("solana settings: rpc_provider must be helius or public (got %q)", out.RPCProvider)
	}
	if out.RPCProvider == "public" && out.RPCAPIKey != "" {
		return out, fmt.Errorf("solana settings: rpc_provider public cannot use rpc_api_key")
	}
	return out, nil
}

// ValidateSolanaAccountSettings is the strict manifest-push variant: values of
// known keys must parse AND every key must be on the allowlist, so a typo'd
// key (e.g. rpc_provder) fails the push loudly instead of being stored inert.
func ValidateSolanaAccountSettings(settings map[string]any) error {
	if _, err := ParseSolanaAccountSettings(settings); err != nil {
		return err
	}
	var unknown []string
	for key := range settings {
		if !solanaKnownSettingKeys[strings.ToLower(strings.TrimSpace(key))] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		known := make([]string, 0, len(solanaKnownSettingKeys))
		for key := range solanaKnownSettingKeys {
			known = append(known, key)
		}
		sort.Strings(known)
		return fmt.Errorf("solana settings: unknown key(s) %s (known: %s)", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return nil
}

// ApplyTo overlays the declared settings onto a boot-plane SolanaRailConfig
// (store-wins, #699) and returns a NEW config; base is never mutated. A nil
// base starts from an empty config (standalone: the store is the only plane).
func (s SolanaAccountSettings) ApplyTo(base *SolanaRailConfig) *SolanaRailConfig {
	out := &SolanaRailConfig{}
	if base != nil {
		*out = *base
		if base.Tokens != nil {
			out.Tokens = make(map[string]TokenConfig, len(base.Tokens))
			for k, v := range base.Tokens {
				out.Tokens[k] = v
			}
		}
	}
	if s.RPCProvider != "" {
		out.RPCProvider = s.RPCProvider
	}
	if s.RPCAPIKey != "" {
		out.RPCAPIKey = s.RPCAPIKey
	}
	if len(s.Tokens) > 0 {
		out.Tokens = make(map[string]TokenConfig, len(s.Tokens))
		for k, v := range s.Tokens {
			out.Tokens[k] = v
		}
	}
	return out
}

func settingString(key string, raw any) (string, error) {
	v, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("solana settings: %s must be a string (got %T)", key, raw)
	}
	return strings.TrimSpace(v), nil
}

// settingTokens decodes the tokens map. Values arrive as generic maps from
// either the YAML manifest or the stored evidence JSON.
func settingTokens(raw any) (map[string]TokenConfig, error) {
	entries, err := settingAnyMap("tokens", raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TokenConfig, len(entries))
	for symbol, rawToken := range entries {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" {
			return nil, fmt.Errorf("solana settings: tokens has an empty symbol key")
		}
		fields, err := settingAnyMap("tokens."+symbol, rawToken)
		if err != nil {
			return nil, err
		}
		var token TokenConfig
		for key, value := range fields {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "mint":
				if token.Mint, err = settingString("tokens."+symbol+".mint", value); err != nil {
					return nil, err
				}
			case "name":
				if token.Name, err = settingString("tokens."+symbol+".name", value); err != nil {
					return nil, err
				}
			case "decimals":
				if token.Decimals, err = settingInt("tokens."+symbol+".decimals", value); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("solana settings: tokens.%s has unknown field %q (want mint, name, decimals)", symbol, key)
			}
		}
		if strings.TrimSpace(token.Mint) == "" {
			return nil, fmt.Errorf("solana settings: tokens.%s requires mint", symbol)
		}
		if token.Decimals < 0 {
			return nil, fmt.Errorf("solana settings: tokens.%s decimals must be >= 0", symbol)
		}
		out[symbol] = token
	}
	return out, nil
}

func settingAnyMap(key string, raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case map[any]any: // legacy YAML decoders
		out := make(map[string]any, len(v))
		for k, val := range v {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("solana settings: %s has a non-string key %v", key, k)
			}
			out[ks] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("solana settings: %s must be a map (got %T)", key, raw)
	}
}

func settingInt(key string, raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case uint64:
		return int(v), nil
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("solana settings: %s must be an integer (got %v)", key, v)
		}
		return int(v), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("solana settings: %s must be an integer (got %q)", key, v)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("solana settings: %s must be an integer (got %T)", key, raw)
	}
}
