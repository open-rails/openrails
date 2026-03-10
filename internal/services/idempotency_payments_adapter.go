package services

import (
	"context"
	"encoding/json"

	"github.com/open-rails/openrails/internal/modules/payments"
)

type PaymentsIdempotencyAdapter struct {
	service *IdempotencyService
}

func NewPaymentsIdempotencyAdapter(service *IdempotencyService) *PaymentsIdempotencyAdapter {
	if service == nil {
		return nil
	}
	return &PaymentsIdempotencyAdapter{service: service}
}

func (a *PaymentsIdempotencyAdapter) Begin(ctx context.Context, operation, key string) (*payments.IdempotencyRecord, bool, error) {
	rec, exists, err := a.service.Begin(ctx, operation, key)
	if err != nil || !exists || rec == nil {
		return nil, exists, err
	}
	return &payments.IdempotencyRecord{
		Status:    payments.IdempotencyStatus(rec.Status),
		Result:    rec.Result,
		Error:     rec.Error,
		CreatedAt: rec.CreatedAt,
	}, true, nil
}

func (a *PaymentsIdempotencyAdapter) Fail(ctx context.Context, operation, key string, operationErr error) error {
	return a.service.Fail(ctx, operation, key, operationErr)
}

func (a *PaymentsIdempotencyAdapter) Complete(ctx context.Context, operation, key string, result any) error {
	switch payload := result.(type) {
	case json.RawMessage:
		return a.service.Complete(ctx, operation, key, payload)
	default:
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return a.service.Complete(ctx, operation, key, encoded)
	}
}
