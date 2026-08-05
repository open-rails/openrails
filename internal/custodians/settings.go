package custodians

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Settings is the typed view of ONE custodian entry's `settings` block. It is
// kind-agnostic on purpose: every kind's knobs are declared in its Descriptor,
// so a second vendor adds rows to the registry, not a second parser.
type Settings struct {
	// PublicAPIKey is the checkout-page key (public by nature, not a secret).
	PublicAPIKey string
	// NetworkTokens arms network-token provisioning at instrument creation.
	NetworkTokens bool
}

// ParseSettings validates a custodian entry's settings against its kind and
// returns the typed view. Unknown keys, wrong types and missing required
// values all fail LOUDLY — a custody knob that is stored inert reads as
// "this works" to the next operator, on a money path.
func ParseSettings(kind string, settings map[string]any) (Settings, error) {
	d, err := Require(kind)
	if err != nil {
		return Settings{}, err
	}
	var unknown []string
	for key := range settings {
		norm := strings.ToLower(strings.TrimSpace(key))
		if _, ok := d.Setting(norm); !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Settings{}, fmt.Errorf("custodian %s: unknown setting(s) %s (known: %s)",
			d.Kind, strings.Join(unknown, ", "), strings.Join(d.SettingNames(), ", "))
	}
	out := Settings{}
	for _, slot := range d.Settings {
		raw, declared := lookup(settings, slot.Name)
		if !declared {
			if slot.Required {
				return Settings{}, fmt.Errorf("custodian %s: %s is required", d.Kind, slot.Name)
			}
			continue
		}
		switch slot.Kind {
		case ValueString:
			v, err := asString(d.Kind, slot.Name, raw)
			if err != nil {
				return Settings{}, err
			}
			if v == "" && slot.Required {
				return Settings{}, fmt.Errorf("custodian %s: %s is required", d.Kind, slot.Name)
			}
			switch slot.Name {
			case SettingPublicAPIKey:
				out.PublicAPIKey = v
			}
		case ValueBool:
			v, err := asBool(d.Kind, slot.Name, raw)
			if err != nil {
				return Settings{}, err
			}
			switch slot.Name {
			case SettingNetworkTokens:
				out.NetworkTokens = v
			}
		}
	}
	return out, nil
}

// PublicSettings projects the entry's settings onto the browser-safe subset
// declared by its kind — the ONLY path by which a custodian setting reaches an
// unauthenticated response. ok=false names a required public value the
// operator never declared, so the caller can omit the PSP rather than
// advertise one a frontend cannot drive (#651).
func (d Descriptor) PublicSettings(settings map[string]any) (map[string]string, string, bool) {
	out := map[string]string{}
	for _, slot := range d.Settings {
		if !slot.Public {
			continue
		}
		raw, _ := lookup(settings, slot.Name)
		v, err := asString(d.Kind, slot.Name, raw)
		if err != nil {
			return nil, err.Error(), false
		}
		if v == "" {
			if slot.Required {
				return nil, fmt.Sprintf("custodian %s declares no %s", d.Kind, slot.Name), false
			}
			continue
		}
		out[slot.PublicField] = v
	}
	return out, "", true
}

func lookup(settings map[string]any, name string) (any, bool) {
	for key, raw := range settings {
		if strings.ToLower(strings.TrimSpace(key)) == name {
			return raw, true
		}
	}
	return nil, false
}

func asString(kind, name string, raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	v, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("custodian %s: %s must be a string (got %T)", kind, name, raw)
	}
	return strings.TrimSpace(v), nil
}

func asBool(kind, name string, raw any) (bool, error) {
	switch v := raw.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return false, fmt.Errorf("custodian %s: %s must be a boolean (got %q)", kind, name, v)
		}
		return b, nil
	default:
		return false, fmt.Errorf("custodian %s: %s must be a boolean (got %T)", kind, name, raw)
	}
}
