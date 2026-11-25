package paymentpage

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// Framing frames an embedded payment page's page, /pay/{invoice_id}, by the origins
// its merchant registered. The invoice id is not a credential: it only picks
// which registered origins may frame the page. A lookup that fails frames
// nothing.
func (s *Store) Framing(settings db.MerchantConfig) Framing {
	return func(r *http.Request) []string {
		if r.Method != http.MethodGet || s == nil || s.db == nil || settings == nil {
			return nil
		}
		invoice, ok := pageInvoice(r)
		if !ok {
			return nil
		}
		ctx := r.Context()
		mids, err := s.db.GenDirectory().ResolveEmbeddedPayURLMerchant(ctx, invoice.UUID())
		if err != nil {
			s.Log.WarnContext(ctx, "payment page: cannot resolve an embedded invoice's merchant", logx.Err(err))
			return nil
		}
		if len(mids) != 1 {
			return nil
		}
		mid := billing.MerchantID(mids[0])
		_, doc, err := settings.MerchantSettings(merchant.WithID(ctx, mid), mid)
		if err != nil {
			s.Log.WarnContext(ctx, "payment page: cannot read the merchant's embed origins", logx.Err(err), "merchant_id", mid.String())
			return nil
		}
		return merchantconfig.EmbedOrigins(doc)
	}
}

// pageInvoice is the invoice a page's path names.
func pageInvoice(r *http.Request) (billing.InvoiceID, bool) {
	raw, ok := strings.CutPrefix(r.URL.Path, PagePrefix)
	if !ok {
		return billing.InvoiceID{}, false
	}
	id, err := billing.ParseInvoiceID(raw)
	if err != nil || id.IsZero() {
		return billing.InvoiceID{}, false
	}
	return id, true
}
