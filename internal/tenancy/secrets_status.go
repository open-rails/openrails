package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ListSecretStatuses returns the registry plus configured/audit state for a
// tenant. It never returns plaintext values.
func (s *Service) ListSecretStatuses(ctx context.Context, id merchant.ID) ([]TenantSecretStatus, error) {
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

	out := make([]TenantSecretStatus, 0, len(tenantSecretRegistry))
	for _, def := range tenantSecretRegistry {
		st := TenantSecretStatus{SecretDefinition: def}
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
		s.applyLatestAudit(ctx, id, &st)
		out = append(out, st)
	}
	return out, nil
}

// DeleteCredential deletes a tenant secret and writes a non-plaintext audit row.
func (s *Service) DeleteCredential(ctx context.Context, id merchant.ID, name, actor string) error {
	if s.secrets == nil {
		return errors.New("tenancy: no secret store configured")
	}
	name = cleanSecretName(name)
	if _, ok := SecretDefinitionFor(name); !ok {
		return fmt.Errorf("tenancy: unknown tenant secret %q", name)
	}
	if err := s.secrets.Delete(ctx, id, name); err != nil {
		return err
	}
	s.audit(ctx, id, name, "delete", actor, "deleted")
	return nil
}

// ValidateCredential validates a supplied or stored credential value without
// returning it. When value is empty, the current stored value is loaded.
func (s *Service) ValidateCredential(ctx context.Context, id merchant.ID, name, value, actor string, stripeTester func(context.Context, string) error) error {
	name = cleanSecretName(name)
	if _, ok := SecretDefinitionFor(name); !ok {
		return fmt.Errorf("tenancy: unknown tenant secret %q", name)
	}
	if strings.TrimSpace(value) == "" {
		if s.secrets == nil {
			return errors.New("tenancy: no secret store configured")
		}
		sec, err := s.secrets.Get(ctx, id, name)
		if err != nil {
			return err
		}
		value = sec.Value
	}

	err := validateSecretValue(ctx, name, value, stripeTester)
	detail := "ok"
	if err != nil {
		detail = "failed:" + validationErrorCode(err)
	}
	s.audit(ctx, id, name, "test", actor, detail)
	return err
}

func validateSecretValue(ctx context.Context, name, value string, stripeTester func(context.Context, string) error) error {
	if err := validateSecretValueLocal(name, value); err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if name == SecretStripeSecretKey {
		if stripeTester == nil {
			stripeTester = defaultStripeBalanceCheck
		}
		return stripeTester(ctx, value)
	}
	return nil
}

func validateSecretValueLocal(name, value string) error {
	name = cleanSecretName(name)
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty")
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
	}
	return nil
}

func validationErrorCode(err error) string {
	if err == nil {
		return ""
	}
	code := strings.TrimSpace(err.Error())
	if code == "" {
		return "failed"
	}
	code = strings.ReplaceAll(code, " ", "_")
	if len(code) > 64 {
		code = code[:64]
	}
	return code
}

func (s *Service) applyLatestAudit(ctx context.Context, id merchant.ID, st *TenantSecretStatus) {
	if s == nil || s.pool == nil || st == nil {
		return
	}
	var action, actor, detail string
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT action, COALESCE(actor,''), COALESCE(detail,''), created_at
		  FROM openrails.tenant_credential_audit
		 WHERE tenant_id = $1::uuid AND name = $2
		 ORDER BY created_at DESC
		 LIMIT 1
	`, id.String(), st.Name).Scan(&action, &actor, &detail, &created)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			st.LastErrorCode = "audit_unavailable"
		}
		return
	}
	if actor != "" {
		st.LastActor = actor
	}
	switch action {
	case "put", "rotate", "delete":
		st.LastRotatedAt = &created
	case "test":
		st.ValidatedAt = &created
	}
	if strings.HasPrefix(detail, "failed:") {
		st.LastErrorCode = strings.TrimPrefix(detail, "failed:")
	}
}
