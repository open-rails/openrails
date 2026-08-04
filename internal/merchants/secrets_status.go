package merchants

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ListSecretStatuses returns one status per PSP-scoped secret the merchant
// actually holds, described from the rail credential registry (#884). It never
// returns plaintext values.
//
// It advertises what is READ, not a static catalogue: a merchant's credential
// slots are a function of the PSPs it declared, and those per-PSP slots are
// already reported by PaymentProviderDefinitions + PaymentProviderConfig.
// Names are sorted so the view is stable across calls.
func (s *Service) ListSecretStatuses(ctx context.Context, id merchant.ID) ([]MerchantSecretStatus, error) {
	if id.IsZero() {
		return nil, validateSecretRef(id, "x")
	}
	if s.secrets == nil {
		return nil, nil
	}
	names, err := s.secrets.List(ctx, id)
	if err != nil {
		return nil, err
	}
	cleaned := make([]string, 0, len(names))
	for _, n := range names {
		if c := cleanSecretName(n); c != "" {
			cleaned = append(cleaned, c)
		}
	}
	sort.Strings(cleaned)

	out := make([]MerchantSecretStatus, 0, len(cleaned))
	seen := map[string]struct{}{}
	for _, name := range cleaned {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		rail, _, _, key, ok, err := ParsePSPSecretName(name)
		if !ok || err != nil {
			// A stored name that is not a PSP-scoped credential slot is not a
			// credential OpenRails reads — never advertise it as one.
			continue
		}
		st := MerchantSecretStatus{
			Name:             name,
			Rail:             rail,
			Key:              key,
			DisplayLabel:     rail + " " + key,
			MerchantWritable: SecretWritable(name),
			Configured:       true,
		}
		if sec, err := s.secrets.Get(ctx, id, name); err == nil {
			st.Version = sec.Version
		} else if !errors.Is(err, ErrSecretNotFound) {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// DeleteCredential deletes a merchant secret.
func (s *Service) DeleteCredential(ctx context.Context, id merchant.ID, name string) error {
	if s.secrets == nil {
		return errors.New("merchants: no secret store configured")
	}
	name = cleanSecretName(name)
	if !SecretWritable(name) {
		return fmt.Errorf("merchants: unknown merchant secret %q", name)
	}
	if err := s.secrets.Delete(ctx, id, name); err != nil {
		return err
	}
	return nil
}

// ValidateCredential validates a supplied or stored credential value without
// returning it. When value is empty, the current stored value is loaded.
func (s *Service) ValidateCredential(ctx context.Context, id merchant.ID, name, value string, stripeTester func(context.Context, string) error) error {
	name = cleanSecretName(name)
	if !SecretWritable(name) {
		return fmt.Errorf("merchants: unknown merchant secret %q", name)
	}
	if strings.TrimSpace(value) == "" {
		if s.secrets == nil {
			return errors.New("merchants: no secret store configured")
		}
		sec, err := s.secrets.Get(ctx, id, name)
		if err != nil {
			return err
		}
		value = sec.Value
	}

	return validateSecretValue(ctx, name, value, stripeTester)
}

func validateSecretValue(ctx context.Context, name, value string, stripeTester func(context.Context, string) error) error {
	if err := validateSecretValueLocal(name, value); err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if isStripeSecretKeyName(name) {
		if stripeTester == nil {
			stripeTester = defaultStripeBalanceCheck
		}
		return stripeTester(ctx, value)
	}
	return nil
}

func isStripeSecretKeyName(name string) bool {
	rail, _, _, key, ok, err := ParsePSPSecretName(name)
	return ok && err == nil && rail == "stripe" && key == "secret_key"
}

// validateSecretValueLocal applies per-(rail, key) format rules to a
// PSP-scoped credential. Names are always scoped (#884), so the rules key off
// the parsed rail/key rather than a second flat spelling.
func validateSecretValueLocal(name, value string) error {
	value = strings.TrimSpace(value)
	rail, _, _, key, scoped, err := ParsePSPSecretName(name)
	if err != nil {
		return err
	}
	if value == "" {
		return errors.New("empty")
	}
	if !scoped {
		return nil
	}
	if rail == "stripe" {
		switch key {
		case "secret_key":
			if !strings.HasPrefix(value, "sk_") {
				return errors.New("invalid_format")
			}
		case "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous":
			if !strings.HasPrefix(value, "whsec_") {
				return errors.New("invalid_format")
			}
		}
	}
	return nil
}
