package merchants

import (
	"reflect"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// RegisterStripePosture registers, when a merchant's configuration loads, the
// posture check of each Stripe PSP document that changed, archived ones too
// (their refunds and cancels still write): it replaces the declaration's
// earlier registration (a corrected account_id or a rotated key), and a
// document gone or holding no key drops its own. Every process that loads
// the merchant does this before it serves the configuration, so a write
// waits only for its PSP's first check.
func RegisterStripePosture(f *stripeapi.Factory, id billing.MerchantID, previous, current merchantdocs.Set) {
	declaration := func(key string) string { return "stripe/" + id.String() + "/" + key }
	for key, doc := range current.PSPs {
		if before, ok := previous.PSPs[key]; ok && reflect.DeepEqual(before.Value, doc.Value) {
			continue
		}
		p := doc.Value
		secret := strings.TrimSpace(p.Secrets["secret_key"])
		if p.Rail != string(models.RailStripe) || secret == "" {
			f.DropPosture(declaration(key))
			continue
		}
		f.RegisterPosture(declaration(key), id.UUID(), PspID(p.Rail, p.Environment, p.AccountID), p.AccountID, secret)
	}
	for key := range previous.PSPs {
		if _, ok := current.PSPs[key]; !ok {
			f.DropPosture(declaration(key))
		}
	}
}
