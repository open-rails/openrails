package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// BasisTheoryWebhookHandler folds Basis Theory custody events into instrument
// state. HTTP ingestion verifies the signature (RSA-PSS vs the CDN key), so
// Verify trusts the ingestion-set flag. The event source is the custodian, not
// a rail. Custody-side problems park the instrument: nothing is terminally
// canceled and nothing rail-side is deleted.
type BasisTheoryWebhookHandler struct{}

func (BasisTheoryWebhookHandler) Rail() string { return string(models.EventSourceBasisTheory) }

func (h BasisTheoryWebhookHandler) Verify(msg *WebhookMessage) error {
	if msg == nil {
		return fmt.Errorf("nil webhook message")
	}
	if !webhookSignatureVerified(msg) {
		return fmt.Errorf("basistheory webhook signature was not verified at ingestion")
	}
	return nil
}

func (h BasisTheoryWebhookHandler) Normalize(msg *WebhookMessage) (WebhookEvent, error) {
	if msg == nil {
		return WebhookEvent{}, fmt.Errorf("nil webhook message")
	}
	var evt basistheory.Event
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return WebhookEvent{}, fmt.Errorf("parse basistheory webhook: %w", err)
	}
	out := WebhookEvent{
		Rail:    string(models.EventSourceBasisTheory),
		Type:    mapBasisTheoryEventType(evt.Type),
		RawType: evt.Type,
		RailRef: evt.ID,
		Raw:     msg.Payload,
	}
	if evt.DeliveredAt != nil {
		out.OccurredAt = evt.DeliveredAt.UTC()
	}
	return out, nil
}

func mapBasisTheoryEventType(t string) WebhookEventType {
	switch t {
	case basistheory.EventTokenUpdated, basistheory.EventNetworkTokenUpdated,
		basistheory.EventTokenDeleted, basistheory.EventTokenExpired,
		basistheory.EventNetworkTokenDeleted:
		return WebhookEventCustomerUpdated
	default:
		return WebhookEventUnknown
	}
}

func (h BasisTheoryWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, msg *WebhookMessage) error {
	if err := h.Verify(msg); err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	var evt basistheory.Event
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("parse basistheory webhook payload: %w", err))
	}
	if strings.TrimSpace(evt.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory webhook has no event id"))
	}
	svc := &basisTheoryWebhookService{d: d, accountID: msg.CustodianAccountID}
	return d.DeduplicationService.ProcessWebhook(ctx, evt.ID, evt.Type, models.EventSourceBasisTheory, func(ctx context.Context) error {
		return svc.apply(ctx, evt)
	})
}

type basisTheoryWebhookService struct {
	d         *WebhookDispatcher
	accountID string
}

// btClient arms the merchant's BT client from the resolved custodian, never a
// PSP: one custodian may back several, and a token event is about the
// instrument, not a gateway.
func (s *basisTheoryWebhookService) btClient(ctx context.Context) (*basistheory.Client, error) {
	if s.d.RailConfigs == nil {
		return nil, fmt.Errorf("basistheory webhook rejected: custodian resolution is not configured")
	}
	cc, err := s.d.RailConfigs.CustodianConfig(ctx, models.CustodianBasisTheory, s.accountID)
	if err != nil {
		return nil, err
	}
	if cc == nil || cc.Custodian != models.CustodianBasisTheory {
		return nil, fmt.Errorf("custodian tenant %s is not declared as basis_theory", s.accountID)
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cc.APIKey,
		BaseURL:       cc.APIBaseURL,
		WebhookKeyURL: cc.WebhookKeyURL,
		ReadOnly:      s.d.Config != nil && config.IsProviderReadOnly(s.d.Config),
	})
}

func custodianScopeIDs(ctx context.Context) (uuid.UUID, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	cid, err := db.RequireCustodianID(ctx)
	return mid.UUID(), cid, err
}

func (s *basisTheoryWebhookService) now() time.Time {
	if s.d.Clock != nil {
		return s.d.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *basisTheoryWebhookService) gen(ctx context.Context) *gen.Queries {
	return s.d.DB.Gen(ctx)
}

func (s *basisTheoryWebhookService) apply(ctx context.Context, evt basistheory.Event) error {
	switch evt.Type {
	case basistheory.EventTokenDeleted, basistheory.EventTokenExpired:
		return s.parkInstrumentFromTokenEvent(ctx, evt)
	case basistheory.EventTokenUpdated:
		return s.refreshInstrumentFromToken(ctx, evt)
	case basistheory.EventTokenPropertyExpired:
		// Expected post-purchase: the intent/token CVC retention lapse.
		return nil
	case basistheory.EventTokenIntentConverted:
		return s.reconcileIntentConversion(ctx, evt)
	case basistheory.EventNetworkTokenUpdated:
		return s.foldNetworkTokenStatus(ctx, evt, "")
	case basistheory.EventNetworkTokenDeleted:
		return s.foldNetworkTokenStatus(ctx, evt, basistheory.NetworkTokenDeleted)
	default:
		log.WithContext(ctx).Debug("basistheory webhook: unhandled event type")
		return nil
	}
}

// parkInstrumentFromTokenEvent parks an instrument whose custodian token is
// gone (deleted or expired). Never terminal-cancel, never delete: subscriptions
// keep their access posture and the operator decides.
func (s *basisTheoryWebhookService) parkInstrumentFromTokenEvent(ctx context.Context, evt basistheory.Event) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.TokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s event carries no token id", evt.Type))
	}
	reason := "bt_" + strings.ReplaceAll(evt.Type, ".", "_") // bt_token_deleted | bt_token_expired
	rows, err := s.gen(ctx).ParkPaymentMethodByMethodRef(ctx, gen.ParkPaymentMethodByMethodRefParams{MerchantID: mid, CustodianID: cid,
		Custodian:     models.CustodianBasisTheory,
		RailMethodRef: data.Token.ID,
		ParkReason:    reason,
	})
	if err != nil {
		return fmt.Errorf("park custodian-held instrument for %s: %w", evt.Type, err)
	}
	if len(rows) > 0 {
		// Renewals on a parked instrument fail loudly until re-collection or
		// vault repair.
		log.WithContext(ctx).Error("basistheory webhook: instrument PARKED — custodian token gone; operator action required (never auto-canceled)")
	}
	return nil
}

// refreshInstrumentFromToken re-reads the token and applies the card it now
// holds to every method on it.
func (s *basisTheoryWebhookService) refreshInstrumentFromToken(ctx context.Context, evt basistheory.Event) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.TokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory token.updated event carries no token id"))
	}
	client, err := s.btClient(ctx)
	if err != nil {
		return err
	}
	token, err := client.GetToken(ctx, data.Token.ID)
	if err != nil {
		if basistheory.IsNotFound(err) {
			// Updated-then-deleted race; the delete event parks it.
			return nil
		}
		return fmt.Errorf("basistheory token.updated: fetch token: %w", err)
	}
	methods, err := s.gen(ctx).ListPaymentMethodsByCustodianRef(ctx, gen.ListPaymentMethodsByCustodianRefParams{MerchantID: mid, CustodianID: cid, Custodian: models.CustodianBasisTheory, RailMethodRef: token.ID})
	if err != nil {
		return fmt.Errorf("basistheory token.updated: find instruments: %w", err)
	}
	card := paymentmethods.Card{Card: btCard(token.Card), Fingerprint: token.Fingerprint}
	for _, id := range methods {
		if _, err := applyCardEvent(ctx, s.d.DB, s.d.SubscriptionLifecycleService, paymentmethods.CardEvent{MerchantID: mid, PaymentMethodID: id,
			Source: paymentmethods.SourceProviderRead, EventRef: evt.ID, Card: card, At: s.now()}); err != nil {
			return fmt.Errorf("basistheory token.updated: %w", err)
		}
	}
	return nil
}

// reconcileIntentConversion catches the crash window between a BT conversion
// and the local instrument write: an existing row is a no-op, a missing one is
// surfaced loudly for repair.
func (s *basisTheoryWebhookService) reconcileIntentConversion(ctx context.Context, evt basistheory.Event) error {
	var data basistheory.TokenIntentConvertedData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return nil // conversion events without a token id carry nothing to reconcile
	}
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	_, err = s.gen(ctx).GetPaymentMethodForCustodianToken(ctx, gen.GetPaymentMethodForCustodianTokenParams{
		MerchantID: mid, CustodianID: cid, Custodian: models.CustodianBasisTheory,
		RailMethodRef: data.Token.ID,
	})
	if err == nil {
		return nil
	}
	if !db.IsNotFound(err) {
		return fmt.Errorf("basistheory token-intent.converted: lookup instrument: %w", err)
	}
	log.WithContext(ctx).Warn("basistheory webhook: converted token has no instrument row (purchase crash window); repair via purchase replay or manual re-link")
	return nil
}

// foldNetworkTokenStatus records a network token's status as a card version;
// the card itself is the Account Updater's.
func (s *basisTheoryWebhookService) foldNetworkTokenStatus(ctx context.Context, evt basistheory.Event, forcedStatus string) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.NetworkTokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.NetworkToken.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s event carries no network token id", evt.Type))
	}
	status := forcedStatus
	if status == "" {
		status = strings.TrimSpace(data.NetworkToken.Status)
		if status == "" {
			client, err := s.btClient(ctx)
			if err != nil {
				return err
			}
			nt, err := client.GetNetworkToken(ctx, data.NetworkToken.ID)
			if err != nil {
				return fmt.Errorf("basistheory %s: fetch network token: %w", evt.Type, err)
			}
			status = nt.Status
		}
	}
	if status == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s: no status resolvable for network token %s", evt.Type, data.NetworkToken.ID))
	}
	methods, err := s.gen(ctx).ListPaymentMethodsByNetworkToken(ctx, gen.ListPaymentMethodsByNetworkTokenParams{MerchantID: mid, CustodianID: cid,
		Custodian: models.CustodianBasisTheory, NetworkTokenID: data.NetworkToken.ID})
	if err != nil {
		return fmt.Errorf("basistheory %s: find instruments: %w", evt.Type, err)
	}
	for _, id := range methods {
		if _, err := applyCardEvent(ctx, s.d.DB, s.d.SubscriptionLifecycleService, paymentmethods.CardEvent{MerchantID: mid, PaymentMethodID: id,
			Source: paymentmethods.SourceNetworkToken, EventRef: evt.ID, Card: paymentmethods.Card{NetworkTokenStatus: status}, At: s.now()}); err != nil {
			return fmt.Errorf("basistheory %s: fold network token status: %w", evt.Type, err)
		}
	}
	return nil
}

// applyCardEvent applies one card event in its own transaction and delivers
// the notices it queued once it commits.
func applyCardEvent(ctx context.Context, database *db.DB, life *subscriptions.SubscriptionLifecycleService, ev paymentmethods.CardEvent) (paymentmethods.CardLifecycle, error) {
	if life == nil {
		return paymentmethods.CardLifecycle{}, errors.New("card lifecycle: no lifecycle service wired")
	}
	var applied paymentmethods.CardLifecycle

	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, _, err = life.ApplyCardLifecycle(ctx, database.NewWithPgxTx(tx), ev)
		return err
	})
	if err != nil {
		return applied, err
	}
	return applied, nil
}

// btCard is a Basis Theory token's card as a stored method's display facts.
func btCard(c *basistheory.CardDetails) models.Card {
	if c == nil {
		return models.Card{}
	}
	card := models.ParseCard(c.Brand, c.Last4, "")
	if c.ExpirationMonth >= 1 && c.ExpirationMonth <= 12 && c.ExpirationYear >= 2000 {
		card.ExpMonth, card.ExpYear = c.ExpirationMonth, c.ExpirationYear
	}
	return card
}

// BasisTheoryWebhookKeyURL selects BT's CDN webhook public key for the
// custodian's money class.
func BasisTheoryWebhookKeyURL(class string) string {
	if class != config.ClassLive {
		return basistheory.TestWebhookKeyURL
	}
	return basistheory.ProdWebhookKeyURL
}
