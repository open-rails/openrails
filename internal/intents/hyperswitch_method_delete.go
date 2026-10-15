package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-rails/openrails/internal/destructive"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const TypeHyperSwitchMethodDelete = "hyperswitch_method_delete"

type HyperSwitchMethodDeletePayload struct {
	CustomerID      uuid.UUID                 `json:"customer_id"`
	PaymentMethodID uuid.UUID                 `json:"payment_method_id"`
	Instrument      charge.FrozenInstrument   `json:"instrument"`
	Binding         charge.HyperSwitchBinding `json:"binding"`
	Environment     string                    `json:"environment"`
	DetachOnly      bool                      `json:"detach_only"`
}

func DecodeHyperSwitchMethodDelete(in gen.BillingProviderIntent) (HyperSwitchMethodDeletePayload, error) {
	var p HyperSwitchMethodDeletePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeHyperSwitchMethodDelete || in.Rail != models.CustodianHyperSwitch || in.PspID != nil || in.SubscriptionID != nil || in.PaymentID != nil || in.PriceID != nil || (in.Origin != string(OriginUser) && in.Origin != string(OriginAdmin)) || in.CustodianID == nil || p.CustomerID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Instrument.Custodian != models.CustodianHyperSwitch || p.Instrument.CustodianID == nil || *p.Instrument.CustodianID != *in.CustodianID || p.Instrument.RailCustomerRef == "" || p.Instrument.RailMethodRef == "" || p.Environment != "test" && p.Environment != "live" || !methodDeleteKeyMatches(TypeHyperSwitchMethodDelete, in.IdempotencyKey, p.PaymentMethodID) {
		return p, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	if err := p.Instrument.ValidateCustody(); err != nil {
		return p, err
	}
	return p, p.Binding.Validate()
}

// The native DELETE contract alone permits exact retries after uncertainty.
// Only a later explicit request can repeat it through the mutation gates.
type HyperSwitchMethodDeleteHandler struct {
	DB    *db.DB
	Rails *paymentmethods.RailPaymentMethodService
	Clock clockwork.Clock
}

func NewHyperSwitchMethodDeleteHandler(d *db.DB, rails *paymentmethods.RailPaymentMethodService, clock clockwork.Clock) *HyperSwitchMethodDeleteHandler {
	return &HyperSwitchMethodDeleteHandler{DB: d, Rails: rails, Clock: timeutil.FirstClock(clock)}
}
func (*HyperSwitchMethodDeleteHandler) Type() string                  { return TypeHyperSwitchMethodDelete }
func (*HyperSwitchMethodDeleteHandler) Backoff(n int32) time.Duration { return DefaultBackoff.Delay(n) }
func (*HyperSwitchMethodDeleteHandler) PrunePolicy() (bool, bool)     { return true, true }
func (*HyperSwitchMethodDeleteHandler) CommitsTerminalOutcome() bool  { return true }
func (*HyperSwitchMethodDeleteHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *HyperSwitchMethodDeleteHandler) Verify(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous("cannot load accepted deletion")
	}
	if _, err := DecodeHyperSwitchMethodDelete(current); err != nil {
		return Ambiguous("invalid accepted deletion")
	}
	if current.Status == StatusSucceeded {
		return Succeeded(nil)
	}
	return Retryable("qualified native deletion permits exact retry through write gates")
}
func deletionBinding(row merchants.CustodianScope, cfg *config.Config) (charge.HyperSwitchBinding, error) {
	if cfg == nil || cfg.HyperSwitch == nil || row.Kind != models.CustodianHyperSwitch || row.Environment == config.ClassLive && cfg.HyperSwitch.AllowLoopbackHTTP {
		return charge.HyperSwitchBinding{}, hyperswitch.ErrBinding
	}
	parsed, err := custodians.ParseSettings(row.Kind, row.Settings)
	if err != nil {
		return charge.HyperSwitchBinding{}, err
	}
	base, err := charge.CanonicalHyperSwitchDeployment(cfg.HyperSwitch.APIBaseURL)
	if err != nil {
		return charge.HyperSwitchBinding{}, err
	}
	b := charge.HyperSwitchBinding{AccountID: row.AccountID, ProfileID: parsed.ProfileID, APIBaseURL: base}
	return b, b.Validate()
}
func (h *HyperSwitchMethodDeleteHandler) Execute(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous("cannot load accepted deletion")
	}
	p, err := DecodeHyperSwitchMethodDelete(current)
	if err != nil {
		return Ambiguous("invalid accepted deletion")
	}
	if current.Status == StatusSucceeded {
		return Succeeded(nil)
	}
	if current.Status != StatusInFlight && current.Status != StatusUnknownNeedsVerify {
		return Ambiguous("deletion is not claimed")
	}
	if h.Rails == nil {
		return parkedBeforeEffect("custodian deletion is unavailable")
	}
	if err := h.checkFence(ctx, current, p); err != nil {
		return parkedBeforeEffect("accepted deletion fence is inconsistent")
	}
	if p.DetachOnly {
		ctx, err = startProtectedEffect(ctx, h.DB, current, h.Clock)
		if err != nil {
			return parkedBeforeEffect(err.Error())
		}
		if err := h.complete(ctx, current, p); err != nil {
			return Ambiguous("local alias detachment pending")
		}
		return Succeeded(deletionEvidence(p))
	}
	configuration := merchants.Of(h.DB)
	if configuration == nil {
		return parkedBeforeEffect("merchant configuration is unavailable")
	}
	row, ok, err := configuration.CustodianScopeByID(ctx, billing.MerchantID(current.MerchantID), *current.CustodianID)
	if err != nil || !ok || row.Environment != p.Environment {
		return parkedBeforeEffect("accepted custodian account is unavailable")
	}
	binding, err := deletionBinding(row, h.Rails.Config)
	if err != nil || binding != p.Binding {
		return parkedBeforeEffect("accepted custodian binding changed")
	}
	client, err := railresolve.HyperSwitchClient(h.Rails.Config, billing.MerchantID(current.MerchantID), row)
	if err != nil {
		return parkedBeforeEffect("accepted custodian credentials are unavailable")
	}
	ctx, err = startProtectedEffect(ctx, h.DB, current, h.Clock)
	if err != nil {
		return parkedBeforeEffect(err.Error())
	}
	if err := client.DeleteMethod(ctx, p.Instrument.RailMethodRef); err != nil {
		if errors.Is(err, hyperswitch.ErrUnknown) {
			return Ambiguous("exact native deletion requires retry")
		}
		return parkedBeforeEffect("native deletion capability is unavailable")
	}
	if err := h.complete(ctx, current, p); err != nil {
		return Ambiguous("native deletion completed; local completion pending")
	}
	return Succeeded(deletionEvidence(p))
}
func (h *HyperSwitchMethodDeleteHandler) complete(ctx context.Context, in gen.BillingProviderIntent, p HyperSwitchMethodDeletePayload) error {
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
		evidence, _ := json.Marshal(deletionEvidence(p))
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

func deletionEvidence(p HyperSwitchMethodDeletePayload) map[string]any {
	key := "physically_deleted"
	if p.DetachOnly {
		key = "detached"
	}
	return map[string]any{key: true, "vendor_method_id": p.Instrument.RailMethodRef}
}
