package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
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

func DecodeHyperSwitchMethodDelete(in gen.OpenrailsRailIntent) (HyperSwitchMethodDeletePayload, error) {
	var p HyperSwitchMethodDeletePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeHyperSwitchMethodDelete || in.Rail != models.CustodianHyperSwitch || in.PspID != nil || in.SubscriptionID != nil || in.PaymentID != nil || in.PriceID != nil || (in.Origin != string(OriginUser) && in.Origin != string(OriginAdmin)) || in.CustodianID == nil || p.CustomerID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Instrument.Custodian != models.CustodianHyperSwitch || p.Instrument.CustodianID == nil || *p.Instrument.CustodianID != *in.CustodianID || p.Instrument.RailCustomerRef == "" || p.Instrument.RailMethodRef == "" || p.Environment != "test" && p.Environment != "live" || in.IdempotencyKey != TypeHyperSwitchMethodDelete+":"+p.PaymentMethodID.String() {
		return p, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	return p, p.Binding.Validate()
}

// The native DELETE contract alone permits exact retries after uncertainty.
// Verification only requeues through the runner's ordinary mutation gates.
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
func (*HyperSwitchMethodDeleteHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *HyperSwitchMethodDeleteHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
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
func deletionBinding(row gen.OpenrailsCustodian, cfg *config.Config) (charge.HyperSwitchBinding, error) {
	if cfg == nil || cfg.HyperSwitch == nil || row.Kind != models.CustodianHyperSwitch || row.Environment != config.ExpectedProviderEnvironment(cfg.IsTestMode()) {
		return charge.HyperSwitchBinding{}, hyperswitch.ErrBinding
	}
	var settings map[string]any
	if err := json.Unmarshal(row.Settings, &settings); err != nil {
		return charge.HyperSwitchBinding{}, err
	}
	parsed, err := custodians.ParseSettings(row.Kind, settings)
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
func (h *HyperSwitchMethodDeleteHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
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
		return Parked("custodian deletion is unavailable")
	}
	if err := h.checkFence(ctx, current, p); err != nil {
		return Parked("accepted deletion fence is inconsistent")
	}
	if p.DetachOnly {
		if err := h.complete(ctx, current, p); err != nil {
			return Ambiguous("local alias detachment pending")
		}
		return Succeeded(deletionEvidence(p))
	}
	row, err := h.DB.Gen(ctx).GetCustodian(ctx, gen.GetCustodianParams{MerchantID: current.MerchantID, ID: *current.CustodianID})
	if err != nil || row.MerchantID != current.MerchantID || row.Environment != p.Environment {
		return Parked("accepted custodian account is unavailable")
	}
	binding, err := deletionBinding(row, h.Rails.Config)
	if err != nil || binding != p.Binding {
		return Parked("accepted custodian binding changed")
	}
	client, err := railresolve.HyperSwitchClient(ctx, h.Rails.Config, h.Rails.MerchantSecrets, merchant.ID(current.MerchantID), row)
	if err != nil {
		return Parked("accepted custodian credentials are unavailable")
	}
	if err := client.DeleteMethod(ctx, p.Instrument.RailMethodRef); err != nil {
		if errors.Is(err, hyperswitch.ErrUnknown) {
			return Ambiguous("exact native deletion requires retry")
		}
		return Parked("native deletion capability is unavailable")
	}
	if err := h.complete(ctx, current, p); err != nil {
		return Ambiguous("native deletion completed; local completion pending")
	}
	return Succeeded(deletionEvidence(p))
}
func (h *HyperSwitchMethodDeleteHandler) complete(ctx context.Context, in gen.OpenrailsRailIntent, p HyperSwitchMethodDeletePayload) error {
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
		if method.CustomerID != p.CustomerID || method.ParkReason != "delete:"+in.ID.String() || p.Instrument.Matches(method, charge.AgreementUnscheduled) != nil {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		n, err := q.DeleteFencedPaymentMethod(ctx, gen.DeleteFencedPaymentMethodParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, OperationID: in.ID})
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
		return nil
	})
}

func deletionEvidence(p HyperSwitchMethodDeletePayload) map[string]any {
	key := "physically_deleted"
	if p.DetachOnly {
		key = "detached"
	}
	return map[string]any{key: true, "vendor_method_id": p.Instrument.RailMethodRef}
}
