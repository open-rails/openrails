package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/billingauth"
)

func paymentMethodDeleteAuthority(ctx context.Context, payer uuid.UUID) (Origin, string) {
	if user, ok := billingauth.FromContext(ctx); ok {
		actor, err := uuid.Parse(user.UserID)
		if err == nil && actor == payer && payer != uuid.Nil {
			return OriginUser, actor.String()
		}
	}
	return OriginAdmin, ResolveActor(ctx, "")
}

// User origin is an authenticated payer decision, never a caller-selected
// label. Canonical admission checks this before stamping the durable actor.
func validatePaymentMethodDeleteAuthority(ctx context.Context, p EnqueueParams) error {
	if p.Origin != OriginUser {
		return nil
	}
	var coordinates struct {
		UserID     string    `json:"user_id"`
		CustomerID uuid.UUID `json:"customer_id"`
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &coordinates); err != nil {
		return err
	}
	payer := coordinates.CustomerID
	if p.IntentType == TypeNMIPaymentMethodDelete {
		payer, err = uuid.Parse(coordinates.UserID)
		if err != nil {
			return err
		}
	}
	origin, actor := paymentMethodDeleteAuthority(ctx, payer)
	if origin != OriginUser || p.Actor != "" && p.Actor != actor {
		return errors.New("user payment-method deletion requires its authenticated payer")
	}
	return nil
}

// Admission establishes this attribution from verified context and locked
// ownership. Recovery needs no live caller session; operator/system work and
// malformed or unattributed rows retain the maintenance gate.
func isSelfServicePaymentMethodDelete(in gen.OpenrailsRailIntent) bool {
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.Origin != string(OriginUser) || in.Actor == nil {
		return false
	}
	actor, err := uuid.Parse(*in.Actor)
	if err != nil || actor == uuid.Nil {
		return false
	}
	switch in.IntentType {
	case TypeHyperSwitchMethodDelete:
		p, err := DecodeHyperSwitchMethodDelete(in)
		return err == nil && p.CustomerID == actor
	case TypeNMIPaymentMethodDelete:
		p, err := decodeNMIVaultDeletePayload(in)
		if err != nil || in.PspID == nil || *in.PspID == uuid.Nil || in.CustodianID != nil || in.IdempotencyKey != NMIPaymentMethodDeleteIdempotencyKey(p.PaymentMethodID) {
			return false
		}
		payer, err := uuid.Parse(p.UserID)
		return err == nil && payer == actor
	}
	return false
}
