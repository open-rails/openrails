package webhooks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// acuAdvice maps NMI's Account Updater notices that carry no card.
var acuAdvice = map[string]paymentmethods.CardAdvice{
	EventTypeNMIACUClosedAccount:   paymentmethods.AdviceClosed,
	EventTypeNMIACUContactCustomer: paymentmethods.AdviceContactCardholder,
}

// handleACUEvent applies an NMI Account Updater notice to each stored card on
// one vault through ApplyCardLifecycle (#1115, #1168):
//   - updated: the card takes the details NMI now holds, read from the vault
//     rather than the notice. A same-brand reissue keeps billing; another
//     brand holds the card's mandates for the customer's consent;
//   - closed account: the card closes and its mandates end;
//   - contact customer: the customer is prompted, or, on a Mastercard card
//     (whose updater means a closed account by it), the card closes.
//
// A notice is one version per card; a redelivery changes nothing.
func (s *NMIWebhookService) handleACUEvent(ctx context.Context) error {
	body, err := s.parseACUEventBody()
	if err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	vault := body.Vault()
	if vault == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("nmi %s names no vault", s.Data.EventType))
	}
	if s.SubscriptionLifecycleService == nil {
		return fmt.Errorf("nmi account updater: no lifecycle service wired")
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
	advice, isAdvice := acuAdvice[s.Data.EventType]
	var customer nmi.V5Customer
	if !isAdvice {
		var found bool
		if customer, found, err = s.readVault(ctx, mid.UUID(), psp, vault); err != nil {
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
		notices = nil
		for _, m := range methods {
			ev := paymentmethods.CardEvent{MerchantID: mid.UUID(), PaymentMethodID: m.ID, Source: paymentmethods.SourceNMIACU, EventRef: s.Data.EventID, Advice: advice, At: now}
			if !isAdvice {
				card, err := intents.NMIVaultCard(customer, models.DerefStr(m.RailMethodRef))
				if err != nil {
					return fmt.Errorf("nmi account updater: card %s: %w", m.ID, err)
				}
				ev.Card = paymentmethods.Card{Card: card}
			}
			_, asked, err := s.SubscriptionLifecycleService.ApplyCardLifecycle(ctx, d, ev)
			if err != nil {
				return err
			}
			notices = append(notices, asked...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.SubscriptionLifecycleService.DispatchNotifications(ctx, notices)
	logger.Info("NMI account updater notice applied")
	return nil
}

// readVault reads one vault from the PSP that holds it.
func (s *NMIWebhookService) readVault(ctx context.Context, merchantID, psp uuid.UUID, vault string) (nmi.V5Customer, bool, error) {
	if s.NMIResolver == nil {
		return nmi.V5Customer{}, false, fmt.Errorf("nmi account updater: no NMI client resolver wired")
	}
	client, ok, err := s.NMIResolver.ResolveNMIClient(ctx, merchantID, &psp)
	if err != nil || !ok || client == nil {
		return nmi.V5Customer{}, false, fmt.Errorf("nmi account updater: client for %s unavailable: %v", psp, err)
	}
	return client.GetCustomer(ctx, vault)
}
