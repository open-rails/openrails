package intents

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/merchant"
)

func intentHistoryAttribution(in gen.BillingProviderIntent, executor string) history.Attribution {
	a := history.Attribution{Origin: in.Origin, Executor: executor, OperationID: &in.ID}
	if in.Actor != nil {
		a.Actor = *in.Actor
	}
	return a
}

type historyHoldKey struct{}

func withHistoryHold(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, historyHoldKey{}, reason)
}
func historyHold(ctx context.Context) string {
	reason, _ := ctx.Value(historyHoldKey{}).(string)
	return reason
}

func (s *Store) recordHistoryHold(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.Gen(ctx).RecordSubscriptionOperationHold(ctx, gen.RecordSubscriptionOperationHoldParams{
		MerchantID: mid.UUID(), ID: id, ReasonCode: historyHold(ctx), Executor: history.FromContext(ctx).Executor,
	})
}
