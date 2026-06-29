package merchants

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ListSecretStatuses returns the registry plus configured/audit state for a
// merchant. It never returns plaintext values.
func (s *Service) ListSecretStatuses(ctx context.Context, id merchant.ID) ([]MerchantSecretStatus, error) {
	if id.IsZero() {
		return nil, validateSecretRef(id, "x")
	}
	configured := map[string]struct{}{}
	if s.secrets != nil {
		names, err := s.secrets.List(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			configured[cleanSecretName(n)] = struct{}{}
		}
	}

	out := make([]MerchantSecretStatus, 0, len(merchantSecretRegistry))
	for _, def := range merchantSecretRegistry {
		st := MerchantSecretStatus{SecretDefinition: def}
		if _, ok := configured[def.Name]; ok {
			st.Configured = true
			if s.secrets != nil {
				if sec, err := s.secrets.Get(ctx, id, def.Name); err == nil {
					st.Version = sec.Version
				} else if !errors.Is(err, ErrSecretNotFound) {
					return nil, err
				}
			}
		}
		out = append(out, st)
	}
	for name := range configured {
		if _, ok := SecretDefinitionFor(name); ok {
			continue
		}
		providerType, _, _, key, ok, err := ParseProviderAccountSecretName(name)
		if !ok || err != nil {
			continue
		}
		st := MerchantSecretStatus{
			SecretDefinition: SecretDefinition{
				Name:             name,
				Rail:             providerType,
				Purpose:          key,
				DisplayLabel:     providerType + " " + key,
				ManualVault:      true,
				MerchantWritable: true,
				Validation:       "presence",
			},
			Configured: true,
		}
		if s.secrets != nil {
			if sec, err := s.secrets.Get(ctx, id, name); err == nil {
				st.Version = sec.Version
			} else if !errors.Is(err, ErrSecretNotFound) {
				return nil, err
			}
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
	if cleanSecretName(name) == SecretStripeSecretKey {
		return true
	}
	providerType, _, _, key, ok, err := ParseProviderAccountSecretName(name)
	return ok && err == nil && providerType == "stripe" && key == "secret_key"
}

func validateSecretValueLocal(name, value string) error {
	name = cleanSecretName(name)
	value = strings.TrimSpace(value)
	providerType, _, _, key, scoped, err := ParseProviderAccountSecretName(name)
	if err != nil {
		return err
	}
	if value == "" {
		return errors.New("empty")
	}
	if scoped {
		switch {
		case providerType == "stripe" && key == "secret_key":
			name = SecretStripeSecretKey
		case providerType == "stripe" && key == "webhook_signing_secret":
			name = SecretStripeWebhookSigning
		case providerType == "stripe" && key == "webhook_signing_secret_thin":
			name = SecretStripeWebhookSigningThin
		case providerType == "nmi" && key == "tokenization_url":
			name = SecretNMITokenizationURL
		}
	}
	switch name {
	case SecretStripeSecretKey:
		if !strings.HasPrefix(value, "sk_") {
			return errors.New("invalid_format")
		}
	case SecretStripeWebhookSigning, SecretStripeWebhookSigningThin:
		if !strings.HasPrefix(value, "whsec_") {
			return errors.New("invalid_format")
		}
	case SecretNMITokenizationURL:
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("invalid_format")
		}
	}
	return nil
}
