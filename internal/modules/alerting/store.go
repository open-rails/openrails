package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
)

// ErrWebhookNotFound is an alert webhook the merchant document does not hold.
var ErrWebhookNotFound = errors.New("alerting: alert webhook not found")

// errConfigUnwired means no merchant configuration was bound to the service.
var errConfigUnwired = errors.New("alerting: merchant configuration is not wired")

// store keeps notifications in Postgres and webhooks in the merchant document.
type store struct {
	db     *db.DB
	config *merchantdocs.Cache
}

func newStore(database *db.DB) *store { return &store{db: database} }

func (s *store) listWebhooks(ctx context.Context, ids []uuid.UUID) ([]Webhook, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.config == nil {
		return nil, errConfigUnwired
	}
	set, err := s.config.Get(ctx, mid)
	if err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(set.Merchant.Value.AlertWebhooks))
	for _, hook := range set.Merchant.Value.AlertWebhooks {
		if ids == nil || slices.Contains(ids, hook.ID.UUID()) {
			out = append(out, webhookOf(mid, hook))
		}
	}
	return out, nil
}

func (s *store) getWebhook(ctx context.Context, id uuid.UUID) (Webhook, error) {
	hooks, err := s.listWebhooks(ctx, []uuid.UUID{id})
	if err != nil {
		return Webhook{}, err
	}
	if len(hooks) == 0 {
		return Webhook{}, ErrWebhookNotFound
	}
	return hooks[0], nil
}

// editWebhooks rewrites the merchant document's webhooks at its current
// revision, retrying a lost race.
func (s *store) editWebhooks(ctx context.Context, edit func([]merchantdocs.AlertWebhook) ([]merchantdocs.AlertWebhook, error)) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if s.config == nil {
		return errConfigUnwired
	}
	if !s.config.Writable() {
		return merchants.ErrConfigReadOnly
	}
	for attempt := 0; ; attempt++ {
		set, err := s.config.Reload(ctx, mid)
		if err != nil {
			return err
		}
		doc := set.Merchant.Value
		hooks, err := edit(slices.Clone(doc.AlertWebhooks))
		if err != nil {
			return err
		}
		doc.AlertWebhooks = hooks
		_, err = s.config.PutMerchant(ctx, mid, doc, set.Merchant.Revision)
		if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
			if attempt >= 2 {
				return merchants.ErrRevisionMismatch
			}
			continue
		}
		return err
	}
}

func webhookOf(mid billing.MerchantID, hook merchantdocs.AlertWebhook) Webhook {
	w := Webhook{
		ID: hook.ID.UUID(), MerchantID: mid.UUID(), url: hook.URL,
		Format: WebhookFormat(hook.Format), Enabled: hook.Enabled, CreatedAt: hook.CreatedAt, UpdatedAt: hook.UpdatedAt,
	}
	if name := strings.TrimSpace(hook.Name); name != "" {
		w.Name = &name
	}
	if w.Format == "" {
		w.Format = FormatGeneric
	}
	if parsed, err := url.Parse(hook.URL); err == nil {
		w.DestinationHost = strings.ToLower(parsed.Host)
	}
	return w
}

// marshalJSON encodes v, substituting empty for a nil/zero value.
func marshalJSON(v any, empty string) ([]byte, error) {
	if v == nil {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("alerting: encode jsonb: %w", err)
	}
	if string(b) == "null" {
		if empty == "" {
			return nil, nil
		}
		return []byte(empty), nil
	}
	return b, nil
}
