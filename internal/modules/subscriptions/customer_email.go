package subscriptions

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// maxCustomerEmailBytes is customers_email_check's bound.
const maxCustomerEmailBytes = 320

// FillCustomerEmail records an email seen at signup or at the provider on a
// customer that has none. A declared email stands; a blank or oversized one
// is not recorded.
func FillCustomerEmail(ctx context.Context, q *gen.Queries, customerID uuid.UUID, email string) error {
	email = strings.TrimSpace(email)
	if email == "" || len(email) > maxCustomerEmailBytes {
		return nil
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return q.FillCustomerEmail(ctx, gen.FillCustomerEmailParams{MerchantID: merchantID.UUID(), ID: customerID, Email: email})
}
