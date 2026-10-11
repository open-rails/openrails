package service

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// RevokePaymentAuthority ends the merchant's authority to charge the
// customer's cards without it: every live agreement ends as
// customer_revoked, and the merchant hears of it. Subscriptions are not
// canceled: a renewal runs only under an active agreement, so it waits for
// the customer. Revoking again ends nothing.
func (s *Service) RevokePaymentAuthority(ctx context.Context, customer billing.CustomerID) (billing.PaymentAuthorityRevocation, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.PaymentAuthorityRevocation{}, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.PaymentAuthorityRevocation{}, err
	}
	now := s.now().UTC()
	out := billing.PaymentAuthorityRevocation{MerchantID: mid, CustomerID: customer, RevokedMandates: []billing.MandateID{}, RevokedAt: now}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		rows, err := q.RevokeCustomerMandates(ctx, gen.RevokeCustomerMandatesParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Now: now})
		if err != nil || len(rows) == 0 {
			return err
		}
		for _, row := range rows {
			out.RevokedMandates = append(out.RevokedMandates, billing.MandateID(row.ID))
		}
		data, err := json.Marshal(billing.PaymentAuthorityHostEvent{CustomerID: customer, RevokedMandates: out.RevokedMandates})
		if err != nil {
			return err
		}
		return q.EnqueuePaymentAuthorityRevoked(ctx, gen.EnqueuePaymentAuthorityRevokedParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(),
			OccurredAt: now, Data: data, DedupeKey: string(billing.HostEventCustomerPaymentAuthorityRevoked) + ":" + rows[0].ID.String()})
	})
	return out, err
}
