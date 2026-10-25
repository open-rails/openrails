package orders

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
)

// fundCreditLine delivers the order's frozen service-credit promise. Money paid
// is the line's amount, independently of the credit granted. The caller owns
// the settlement transaction, so payment, grant and renewal advance together.
func fundCreditLine(ctx context.Context, d *db.DB, line gen.BillingOrderLine, paymentID uuid.UUID, now time.Time, clock clockwork.Clock) error {
	credit, err := creditOf(line)
	if err != nil || credit == nil {
		return err
	}
	if err := credit.Validate(); err != nil {
		return err
	}
	quantity := int64(seats(line.Quantity))
	if quantity <= 0 || credit.Amount > math.MaxInt64/quantity {
		return fmt.Errorf("order credit amount overflows")
	}
	expires := now.AddDate(0, 0, credit.ExpiresAfterDays)
	_, err = purchasedcredits.New(d, clock).Fund(ctx, purchasedcredits.Params{
		CustomerID: identity.CustomerID(line.CustomerID), PaymentID: paymentID, ProductID: models.DerefUUID(line.ProductID), OrderLineID: line.ID,
		Amount: credit.Amount * quantity, PaidAmount: line.Amount, Currency: credit.Currency, StartsAt: now, ExpiresAt: &expires,
		Description: line.Description,
	})
	return err
}
