package merchantdocs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/open-rails/openrails/billing"
)

// configurationChange contains only keyed fingerprints, never credential or
// webhook URL values. One target is one independently damaging configuration
// object; changing a destination and its name still counts once.
type configurationChange struct {
	Target string `json:"target"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func configurationFingerprint(key []byte, value any) (string, error) {
	if len(key) != 32 {
		return "", errors.New("configuration admission needs a 32-byte fingerprint key")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("configuration admission fingerprint: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("openrails/destructive-configuration/v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// configurationChanges excludes display-only fields but includes billable,
// access, routing and security settings. Webhook targets count separately from
// merchant settings so a replacement list cannot erase many alert sinks for
// the cost of one scalar edit.
func configurationChanges(key []byte, name string, before, after any) ([]configurationChange, error) {
	out := []configurationChange{}
	compare := func(target string, old, next any) error {
		a, err := configurationFingerprint(key, old)
		if err != nil {
			return err
		}
		b, err := configurationFingerprint(key, next)
		if err != nil {
			return err
		}
		if a != b {
			out = append(out, configurationChange{Target: target, Before: a, After: b})
		}
		return nil
	}
	if name != MerchantDoc {
		if err := compare(name, before, after); err != nil {
			return nil, err
		}
		return out, nil
	}
	old, next := Merchant{}, after.(Merchant)
	if before != nil {
		old = before.(Merchant)
	}
	settings := func(value billing.MerchantSettings) billing.MerchantSettings {
		if value.Profile != nil {
			profile := *value.Profile
			profile.LogoURL = ""
			value.Profile = &profile
			if profile == (billing.MerchantProfile{}) {
				value.Profile = nil
			}
		}
		return value
	}
	if err := compare("merchant/settings", settings(old.Settings), settings(next.Settings)); err != nil {
		return nil, err
	}
	if err := compare("merchant/credentials", old.Secrets, next.Secrets); err != nil {
		return nil, err
	}
	hooks := func(in []AlertWebhook) map[billing.AlertWebhookID]AlertWebhook {
		out := make(map[billing.AlertWebhookID]AlertWebhook, len(in))
		for _, hook := range in {
			out[hook.ID] = hook
		}
		return out
	}
	previous, wanted := hooks(old.AlertWebhooks), hooks(next.AlertWebhooks)
	keys := make([]billing.AlertWebhookID, 0, len(previous)+len(wanted))
	for id := range previous {
		keys = append(keys, id)
	}
	for id := range wanted {
		if _, exists := previous[id]; !exists {
			keys = append(keys, id)
		}
	}
	slices.SortFunc(keys, func(a, b billing.AlertWebhookID) int { return slices.Compare(a[:], b[:]) })
	// Names and write timestamps are cosmetic; removal/creation and destination,
	// format or enablement change operational delivery authority.
	terms := func(h AlertWebhook, exists bool) any {
		if !exists {
			return nil
		}
		return struct {
			URL     string
			Format  billing.AlertWebhookFormat
			Enabled bool
		}{h.URL, h.Format, h.Enabled}
	}
	for _, id := range keys {
		a, aok := previous[id]
		b, bok := wanted[id]
		if err := compare("merchant/alert-webhooks/"+id.String(), terms(a, aok), terms(b, bok)); err != nil {
			return nil, err
		}
	}
	return out, nil
}
