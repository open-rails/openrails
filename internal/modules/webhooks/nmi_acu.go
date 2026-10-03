package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// acuKinds maps NMI's Account Updater notices to card updates.
var acuKinds = map[string]paymentmethods.CardUpdateKind{
	EventTypeNMIACUUpdated:         paymentmethods.CardUpdated,
	EventTypeNMIACUClosedAccount:   paymentmethods.CardClosed,
	EventTypeNMIACUContactCustomer: paymentmethods.ContactCustomer,
}

// handleACUEvent acts on an NMI Account Updater notice for one vault (#1115):
//   - updated: each stored card on the vault takes the details NMI now holds
//     (read from the vault, not the notice), a park an earlier notice set is
//     cleared, and memberships waiting on the card retry at the next due pass;
//   - closed account: the card is parked, so it is not charged again, and its
//     members are asked for a new card;
//   - contact customer: its members are asked for a new card.
//
// Each is recorded once per notice in payment_method_updates.
func (s *NMIWebhookService) handleACUEvent(ctx context.Context) error {
	body, err := s.parseACUEventBody()
	if err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	vault, kind := body.Vault(), acuKinds[s.Data.EventType]
	if vault == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("nmi %s names no vault", s.Data.EventType))
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	methods, err := s.DB.Gen(ctx).ListVaultPaymentMethods(ctx, gen.ListVaultPaymentMethodsParams{MerchantID: mid.UUID(), PspID: psp, RailCustomerRef: vault})
	if err != nil {
		return err
	}
	logger := log.WithContext(ctx).WithFields(log.Fields{"event_type": s.Data.EventType, "vault_id": vault, "methods": len(methods)})
	if len(methods) == 0 {
		logger.Info("NMI account updater notice for a vault with no stored card here")
		return nil
	}
	var customer nmi.V5Customer
	if kind == paymentmethods.CardUpdated {
		if s.NMIResolver == nil {
			return fmt.Errorf("nmi account updater: no NMI client resolver wired")
		}
		client, ok, err := s.NMIResolver.ResolveNMIClient(ctx, mid.UUID(), &psp)
		if err != nil || !ok || client == nil {
			return fmt.Errorf("nmi account updater: client for %s unavailable: %v", psp, err)
		}
		var found bool
		if customer, found, err = client.GetCustomer(ctx, vault); err != nil {
			return err
		}
		if !found {
			logger.Warn("NMI account updater notice for a vault NMI no longer has")
			return nil
		}
	}
	now := s.now()
	var notices []*models.NotificationQueue
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		notices = nil
		for _, m := range methods {
			switch kind {
			case paymentmethods.CardUpdated:
				lastFour, cardType, expiry, err := intents.NMIVaultCard(customer, m.RailMethodRef)
				if err != nil {
					return fmt.Errorf("nmi account updater: card %s: %w", m.ID, err)
				}
				if _, err := q.RefreshPaymentMethodCard(ctx, gen.RefreshPaymentMethodCardParams{MerchantID: mid.UUID(), ID: m.ID,
					LastFour: lastFour, CardType: cardType, ExpiryDate: expiry, UpdatedAt: now}); err != nil {
					return err
				}
				if err := subscriptions.WakeForReplacedMethod(ctx, d, mid.UUID(), m.ID, now); err != nil {
					return err
				}
			case paymentmethods.CardClosed:
				if _, err := q.ParkPaymentMethod(ctx, gen.ParkPaymentMethodParams{MerchantID: mid.UUID(), ID: m.ID, ParkReason: "nmi_acu_closed_account", ParkedAt: now}); err != nil {
					return err
				}
				fallthrough
			case paymentmethods.ContactCustomer:
				asked, err := s.askForNewCard(ctx, d, mid, m.ID, now)
				if err != nil {
					return err
				}
				notices = append(notices, asked...)
			}
			if err := paymentmethods.RecordCardUpdate(ctx, q, paymentmethods.CardUpdate{MerchantID: mid.UUID(), PaymentMethodID: m.ID, CustomerID: m.CustomerID,
				PSPID: m.PspID, Source: paymentmethods.UpdateNMIACU, Kind: kind, EventRef: s.Data.EventID, At: now}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.SubscriptionLifecycleService != nil {
		s.SubscriptionLifecycleService.DispatchNotifications(ctx, notices)
	}
	logger.Info("NMI account updater notice applied")
	return nil
}

// askForNewCard queues the update-your-card notice for each membership the
// card pays for, once per paid period.
func (s *NMIWebhookService) askForNewCard(ctx context.Context, d *db.DB, mid merchant.ID, method uuid.UUID, now time.Time) ([]*models.NotificationQueue, error) {
	if s.SubscriptionLifecycleService == nil {
		return nil, fmt.Errorf("nmi account updater: no lifecycle service wired")
	}
	ids, err := d.Gen(ctx).ListLiveSubscriptionsOnMethod(ctx, gen.ListLiveSubscriptionsOnMethodParams{MerchantID: mid.UUID(), PaymentMethodID: method})
	if err != nil {
		return nil, err
	}
	repo := subscriptions.NewSubscriptionRepo(d)
	var out []*models.NotificationQueue
	for _, id := range ids {
		sub, err := repo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		queued, err := s.SubscriptionLifecycleService.ApplyEffects(ctx, d, sub, []lifecycle.Effect{lifecycle.Notify{Kind: lifecycle.NoticeUpdateMethod}}, now, subscriptions.EffectOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, queued...)
	}
	return out, nil
}
