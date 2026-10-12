package alerting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DestructiveChannels snapshots configured destinations for one durable
// incident. Call outside the admission transaction: configuration may use Vault.
func (s *Service) DestructiveChannels(ctx context.Context) ([]ChannelRef, error) {
	if s == nil || s.store == nil || s.store.config == nil {
		return nil, errors.New("destructive alert service is not configured")
	}
	hooks, err := s.store.listWebhooks(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := []ChannelRef{}
	var recipient string
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		settings, _, err := s.deliverer.cfgStore.Get(ctx)
		if err != nil {
			return err
		}
		recipient = settings.AlertEmail
		return nil
	})
	if err != nil {
		return nil, err
	}
	if recipient != "" {
		out = append(out, ChannelRef{Type: ChannelEmail})
	}
	for _, h := range hooks {
		if h.Enabled {
			id := h.ID
			out = append(out, ChannelRef{Type: ChannelWebhook, WebhookID: &id})
		}
	}
	return out, nil
}

// DeliverDestructiveAlert attempts one incident/channel delivery. Only success
// completes its River job; failures remain retryable. A repeated HTTP delivery
// carries the same idempotency key. Email is at-least-once across a lost ACK.
func (s *Service) DeliverDestructiveAlert(ctx context.Context, hold uuid.UUID, scope string, opened time.Time, channel ChannelRef) error {
	alert := Alert{Title: "Destructive actions held for operator review", Severity: SeverityCritical,
		Summary:       fmt.Sprintf("A %s destructive-action ceiling tripped. New actions remain held until an authorized operator clears incident %s.", scope, hold),
		DashboardLink: s.opsLink("destructive_hold=" + hold.String()), FiredAt: opened}
	if channel.Type == ChannelEmail {
		var recipient string
		err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
			settings, _, err := s.deliverer.cfgStore.Get(ctx)
			if err == nil {
				recipient = settings.AlertEmail
			}
			return err
		})
		if err != nil {
			return err
		}
		if recipient == "" {
			return nil
		} // The operator deliberately removed the destination.
		if s.deliverer.email == nil || !s.deliverer.email.IsEnabled() {
			return errors.New("destructive alert email sender is not configured")
		}
		html, plain := renderEmail(alert)
		if err := s.deliverer.email.SendEmail(ctx, recipient, alertTitle(alert), html, plain); err != nil {
			return errors.New("destructive alert email delivery failed")
		}
		return nil
	}

	if channel.Type != ChannelWebhook || channel.WebhookID == nil {
		return errors.New("invalid destructive alert channel")
	}
	hook, err := s.store.getWebhook(ctx, *channel.WebhookID)
	if errors.Is(err, ErrWebhookNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !hook.Enabled {
		return nil
	}
	body, err := shapeWebhookBody(hook.Format, alert)
	if err != nil {
		return err
	}
	if err = s.outbound.ValidateURL(hook.url); err != nil {
		return errors.New("destructive alert destination refused")
	}
	status, err := s.deliverer.postOnceWithKey(ctx, hook.url, body, "destructive-hold:"+hold.String()+":"+hook.ID.String())
	if err != nil {
		return errors.New("destructive alert webhook delivery failed")
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("destructive alert webhook returned status %d", status)
	}
	return nil
}
