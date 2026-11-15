package payment

import (
	"encoding/json"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
)

func initialMembershipReplayParams(in gen.BillingProviderIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: in.IntentType, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}
