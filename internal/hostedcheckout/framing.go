package hostedcheckout

import (
	"context"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// MerchantSettings reads a merchant's settings document.
type MerchantSettings interface {
	MerchantSettings(ctx context.Context, id billing.MerchantID) (string, billing.MerchantSettings, error)
}

// Framing frames an embedded checkout's page, /c/{order_id}, by the origins
// its merchant registered. The order id is not a credential: it only picks
// which registered origins may frame the page. A lookup that fails frames
// nothing.
func (s *Store) Framing(settings MerchantSettings) Framing {
	return func(r *http.Request) []string {
		if r.Method != http.MethodGet || s == nil || s.db == nil || settings == nil {
			return nil
		}
		order, ok := pageOrder(r)
		if !ok {
			return nil
		}
		ctx := r.Context()
		mids, err := s.db.GenDirectory().ResolveEmbeddedCheckoutMerchant(ctx, order.UUID())
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("hosted checkout: cannot resolve an embedded order's merchant")
			return nil
		}
		if len(mids) != 1 {
			return nil
		}
		mid := billing.MerchantID(mids[0])
		_, doc, err := settings.MerchantSettings(merchant.WithID(ctx, mid), mid)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("hosted checkout: cannot read the merchant's embed origins")
			return nil
		}
		return merchantconfig.EmbedOrigins(doc)
	}
}

// pageOrder is the order a page's path names.
func pageOrder(r *http.Request) (billing.OrderID, bool) {
	raw, ok := strings.CutPrefix(r.URL.Path, "/c/")
	if !ok {
		return billing.OrderID{}, false
	}
	id, err := billing.ParseOrderID(raw)
	if err != nil || id.IsZero() || !strings.HasPrefix(raw, billing.OrderIDPrefix) {
		return billing.OrderID{}, false
	}
	return id, true
}
