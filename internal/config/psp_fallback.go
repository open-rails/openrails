package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// DefaultFallbackAfterFailures is how many failed attempts through its
// primaries unlock a fallback PSP on an order, unless the PSP says otherwise.
const DefaultFallbackAfterFailures = 2

// PSPFallback is a PSP's fallback declaration: an order offers it only while
// none of For can take the order, or once the order has AfterFailures failed
// attempts through them.
type PSPFallback struct {
	For           []string
	AfterFailures int
}

// ReadPSPFallback reads a PSP's fallback_for and fallback_after_failures
// settings; ok is false for a PSP that is no fallback.
func ReadPSPFallback(settings map[string]any) (fallback PSPFallback, ok bool, err error) {
	raw, declared := settings["fallback_for"]
	after, counted := settings["fallback_after_failures"]
	if !declared {
		if counted {
			return PSPFallback{}, false, fmt.Errorf("fallback_after_failures needs fallback_for")
		}
		return PSPFallback{}, false, nil
	}
	var keys []string
	switch v := raw.(type) {
	case []string:
		keys = v
	case []any:
		for _, item := range v {
			text, isText := item.(string)
			if !isText {
				return PSPFallback{}, false, fmt.Errorf("fallback_for lists PSP keys")
			}
			keys = append(keys, text)
		}
	default:
		return PSPFallback{}, false, fmt.Errorf("fallback_for lists PSP keys")
	}
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return PSPFallback{}, false, fmt.Errorf("fallback_for names an empty PSP key")
		}
		if slices.Contains(fallback.For, key) {
			return PSPFallback{}, false, fmt.Errorf("fallback_for repeats %q", key)
		}
		fallback.For = append(fallback.For, key)
	}
	if len(fallback.For) == 0 {
		return PSPFallback{}, false, fmt.Errorf("fallback_for names no PSP")
	}
	fallback.AfterFailures = DefaultFallbackAfterFailures
	if counted {
		n, whole := wholeNumber(after)
		if !whole || n < 1 || n > 100 {
			return PSPFallback{}, false, fmt.Errorf("fallback_after_failures must be a whole number from 1 to 100")
		}
		fallback.AfterFailures = int(n)
	}
	return fallback, true, nil
}

// ValidatePSPFallbacks checks every PSP's fallback declaration against the
// merchant's PSP keys.
func ValidatePSPFallbacks(psps map[string]PSPConfig) error {
	declared := func(key string) bool {
		for k := range psps {
			if strings.EqualFold(strings.TrimSpace(k), key) {
				return true
			}
		}
		return false
	}
	for _, key := range slices.Sorted(maps.Keys(psps)) {
		if err := ValidatePSPFallback(key, psps[key].Settings, declared); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePSPFallback checks one PSP's fallback declaration: it names other
// PSPs the merchant declares.
func ValidatePSPFallback(key string, settings map[string]any, declared func(string) bool) error {
	fallback, ok, err := ReadPSPFallback(settings)
	if err != nil {
		return fmt.Errorf("psps.%s.settings: %w", key, err)
	}
	if !ok {
		return nil
	}
	for _, primary := range fallback.For {
		switch {
		case primary == strings.ToLower(strings.TrimSpace(key)):
			return fmt.Errorf("psps.%s.settings.fallback_for: a PSP is no fallback for itself", key)
		case !declared(primary):
			return fmt.Errorf("psps.%s.settings.fallback_for: %q is no declared PSP", key, primary)
		}
	}
	return nil
}

func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		return int64(min(n, math.MaxInt64)), true
	case float64:
		return int64(n), n == math.Trunc(n) && n >= math.MinInt64 && n <= math.MaxInt64
	}
	return 0, false
}
