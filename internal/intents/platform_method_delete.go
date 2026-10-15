package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const TypePlatformMethodDelete = "platform_method_delete"

type PlatformMethodDeletePayload struct {
	CustomerID      uuid.UUID               `json:"customer_id"`
	PaymentMethodID uuid.UUID               `json:"payment_method_id"`
	Instrument      charge.FrozenInstrument `json:"instrument"`
	TenantID        string                  `json:"tenant_id"`
	Environment     string                  `json:"environment"`
}

func DecodePlatformMethodDelete(in gen.BillingProviderIntent) (PlatformMethodDeletePayload, error) {
	var p PlatformMethodDeletePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil ||
		in.MerchantID == uuid.Nil ||
		in.IntentType != TypePlatformMethodDelete ||
		in.Rail != models.CustodianPlatform ||
		in.PspID != nil ||
		in.SubscriptionID != nil ||
		in.PaymentID != nil ||
		in.PriceID != nil ||
		(in.Origin != string(OriginUser) && in.Origin != string(OriginAdmin)) ||
		in.CustodianID == nil ||
		*in.CustodianID == uuid.Nil ||
		p.CustomerID == uuid.Nil ||
		p.PaymentMethodID == uuid.Nil ||
		p.Instrument.Custodian != models.CustodianPlatform ||
		p.Instrument.CustodianID == nil ||
		*p.Instrument.CustodianID != *in.CustodianID ||
		p.Instrument.PSPID != uuid.Nil ||
		p.Instrument.Mandate != nil ||
		p.Instrument.RailCustomerRef != "" ||
		p.Instrument.RailMethodRef == "" ||
		strings.TrimSpace(p.TenantID) == "" ||
		strings.Contains(p.TenantID, "/") ||
		p.Environment != "test" && p.Environment != "live" ||
		!methodDeleteKeyMatches(TypePlatformMethodDelete, in.IdempotencyKey, p.PaymentMethodID) {
		return p, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	return p, p.Instrument.ValidateCustody()
}

type PlatformMethodDeleteHandler struct {
	DB    *db.DB
	Rails *paymentmethods.RailPaymentMethodService
	Clock clockwork.Clock
}

func NewPlatformMethodDeleteHandler(d *db.DB, rails *paymentmethods.RailPaymentMethodService, clock clockwork.Clock) *PlatformMethodDeleteHandler {
	return &PlatformMethodDeleteHandler{DB: d, Rails: rails, Clock: timeutil.FirstClock(clock)}
}
func (*PlatformMethodDeleteHandler) Type() string                  { return TypePlatformMethodDelete }
func (*PlatformMethodDeleteHandler) Backoff(n int32) time.Duration { return DefaultBackoff.Delay(n) }
func (*PlatformMethodDeleteHandler) PrunePolicy() (bool, bool)     { return true, true }
func (*PlatformMethodDeleteHandler) CommitsTerminalOutcome() bool  { return true }
func (*PlatformMethodDeleteHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *PlatformMethodDeleteHandler) Verify(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	return h.resolve(ctx, in, false)
}
func (h *PlatformMethodDeleteHandler) Execute(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	return h.resolve(ctx, in, true)
}

func (h *PlatformMethodDeleteHandler) resolve(ctx context.Context, in gen.BillingProviderIntent, execute bool) Outcome {
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous("cannot load accepted platform card removal")
	}
	p, err := DecodePlatformMethodDelete(current)
	if err != nil {
		return Ambiguous("invalid accepted platform card removal")
	}
	if current.Status == StatusSucceeded {
		return Succeeded(nil)
	}
	if current.Status != StatusInFlight && current.Status != StatusUnknownNeedsVerify {
		return Ambiguous("platform card removal is not claimed")
	}
	if err := h.checkFence(ctx, current, p); err != nil {
		return Ambiguous("accepted platform card removal fence is inconsistent")
	}
	configuration := merchants.Of(h.DB)
	if configuration == nil || h.Rails == nil {
		return Ambiguous("platform vault configuration is unavailable")
	}
	mid := billing.MerchantID(current.MerchantID)
	account, ok, err := configuration.CustodianScopeByID(ctx, mid, *current.CustodianID)
	if err != nil || !ok || account.Environment != p.Environment {
		return Ambiguous("accepted platform vault account is unavailable")
	}
	cfg, err := configuration.PlatformCustodianConfig(ctx, mid, *current.CustodianID)
	container := "/merchants/" + current.MerchantID.String() + "/"
	if err != nil || cfg.AccountID != p.TenantID || cfg.Container != container {
		return Ambiguous("accepted platform vault binding or credentials are unavailable")
	}
	baseURL := cfg.APIBaseURL
	if h.Rails.BTBaseURLOverride != "" {
		baseURL = h.Rails.BTBaseURLOverride
	}
	client, err := basistheory.New(basistheory.Config{APIKey: cfg.APIKey, BaseURL: baseURL, ReadOnly: h.Rails.Config != nil && config.IsProviderReadOnly(h.Rails.Config)})
	if err != nil {
		return Ambiguous("platform vault client is unavailable")
	}
	tenantID, err := client.TenantID(ctx)
	if err != nil {
		return Ambiguous("platform vault could not confirm the API key's tenant")
	}
	if tenantID != p.TenantID {
		return Ambiguous("platform vault API key belongs to another tenant")
	}
	token, err := client.GetToken(ctx, p.Instrument.RailMethodRef)
	if err != nil && !basistheory.IsNotFound(err) {
		return Ambiguous("platform vault did not confirm whether the card exists")
	}
	if err == nil {
		if token.ID != p.Instrument.RailMethodRef || token.TenantID != p.TenantID || token.Type != "card" || len(token.Containers) != 1 || token.Containers[0] != container {
			return Ambiguous("platform token ownership does not match accepted removal")
		}
		if !execute {
			return Retryable("platform card still exists; retry removal through write controls")
		}
		ctx, err = startProtectedEffect(ctx, h.DB, current, h.Clock)
		if err != nil {
			return parkedBeforeEffect(err.Error())
		}
		if err := client.DeleteToken(ctx, token.ID); err != nil && !basistheory.IsNotFound(err) {
			return Ambiguous("platform vault did not confirm card removal")
		}
	}
	if err := h.complete(ctx, current, p); err != nil {
		return Ambiguous("platform card is absent; local removal is pending")
	}
	return Succeeded(platformDeletionEvidence(p))
}

func platformDeletionEvidence(p PlatformMethodDeletePayload) map[string]any {
	return map[string]any{"verified_absent": true, "vendor_method_id": p.Instrument.RailMethodRef}
}

func (h *PlatformMethodDeleteHandler) complete(ctx context.Context, in gen.BillingProviderIntent, p PlatformMethodDeletePayload) error {
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: *in.CustodianID, Method: p.Instrument.RailMethodRef}
		if err := paymentmethods.LockCustodianHandles(ctx, q, in.MerchantID, handle); err != nil {
			return err
		}
		current, err := q.LockCustodianMethodDelete(ctx, gen.LockCustodianMethodDeleteParams{MerchantID: in.MerchantID, ID: in.ID})
		if err != nil {
			return err
		}
		if !bytes.Equal(current.Payload, in.Payload) || current.CustodianID == nil || *current.CustodianID != *in.CustodianID {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if current.Status == StatusSucceeded {
			return nil
		}
		method, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != p.CustomerID || models.DerefStr(method.ParkReason) != "delete:"+in.ID.String() || p.Instrument.Matches(method) != nil {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if _, err := mandates.EndForPaymentMethod(ctx, q, in.MerchantID, p.PaymentMethodID, mandates.EndPaymentMethodRemoved, h.Clock.Now()); err != nil {
			return err
		}
		n, err := q.RemoveFencedCustodianPaymentMethod(ctx, gen.RemoveFencedCustodianPaymentMethodParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, OperationID: in.ID, Now: h.Clock.Now().UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		evidence, err := json.Marshal(platformDeletionEvidence(p))
		if err != nil {
			return err
		}
		n, err = q.CompleteCustodianMethodDelete(ctx, gen.CompleteCustodianMethodDeleteParams{MerchantID: in.MerchantID, ID: in.ID, Evidence: evidence, Now: h.Clock.Now().UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return finishIntentActionTx(ctx, tx, in, destructive.OutcomeSucceeded)
	})
}
