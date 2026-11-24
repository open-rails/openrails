package orders

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// CheckoutFacts are what a checkout's request tells of its customer: the
// country a trusted proxy reported and the preferred language, both only
// for the customer in person, and the merchant's attribution.
type CheckoutFacts struct {
	IPCountry   string
	Language    string
	Attribution billing.OrderAttribution
}

const maxAttribution = 100

var (
	countryShape  = regexp.MustCompile(`^[A-Z]{2}$`)
	languageShape = regexp.MustCompile(`^[a-z]{2,3}(-([A-Z]{2}|[0-9]{3}))?$`)
	hostShape     = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// Country is s as an ISO 3166-1 alpha-2 code, or "".
func Country(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !countryShape.MatchString(s) {
		return ""
	}
	return s
}

// language is s when it is a language[-region] tag, else "".
func language(s string) string {
	if !languageShape.MatchString(s) {
		return ""
	}
	return s
}

// CheckAttribution is a's fields trimmed, empty ones absent, and the
// referrer reduced to its host. A field over 100 bytes, with a control
// character, "@" or a card number is invalid_param.
func CheckAttribution(a *billing.OrderAttribution) (billing.OrderAttribution, error) {
	if a == nil {
		return billing.OrderAttribution{}, nil
	}
	var out billing.OrderAttribution
	for _, f := range []struct {
		name string
		in   *string
		out  **string
	}{{"source", a.Source, &out.Source}, {"medium", a.Medium, &out.Medium}, {"campaign", a.Campaign, &out.Campaign}, {"affiliate", a.Affiliate, &out.Affiliate}} {
		v, err := attributionText(f.name, f.in)
		if err != nil {
			return billing.OrderAttribution{}, err
		}
		*f.out = v
	}
	referrer, err := referrerHost(a.Referrer)
	if err != nil {
		return billing.OrderAttribution{}, err
	}
	out.Referrer = referrer
	return out, nil
}

func attributionText(name string, in *string) (*string, error) {
	if in == nil || strings.TrimSpace(*in) == "" {
		return nil, nil
	}
	v := strings.TrimSpace(*in)
	param := "attribution." + name
	switch {
	case len(v) > maxAttribution:
		return nil, apperr.Invalidf("%s is at most %d bytes", param, maxAttribution).WithParam(param)
	case !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl):
		return nil, apperr.Invalidf("%s is printable text", param).WithParam(param)
	case strings.Contains(v, "@") || cardguard.ContainsPAN(v):
		return nil, apperr.Invalidf("%s must not carry an email address or card number", param).WithParam(param)
	}
	return &v, nil
}

// referrerHost keeps a referrer URL's host, never its path or query.
func referrerHost(in *string) (*string, error) {
	if in == nil || strings.TrimSpace(*in) == "" {
		return nil, nil
	}
	v := strings.TrimSpace(*in)
	invalid := apperr.Invalidf("attribution.referrer is a URL or an ASCII host name").WithParam("attribution.referrer")
	if len(v) > 2048 {
		return nil, invalid
	}
	host := v
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return nil, invalid
		}
		host = u.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !hostShape.MatchString(host) {
		return nil, invalid
	}
	return &host, nil
}

// MarkOpened records the customer's first read of their open checkout in
// person; later reads change nothing.
func (s *Service) MarkOpened(ctx context.Context, customerID, id uuid.UUID, facts CheckoutFacts) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.Gen(ctx).MarkCheckoutOpened(ctx, gen.MarkCheckoutOpenedParams{MerchantID: mid.UUID(), CustomerID: customerID, ID: id, Now: s.now(),
		IpCountry: optional(Country(facts.IPCountry)), Language: optional(language(facts.Language))})
	return err
}

// MarkSubmitted records a pay request on the customer's checkout, before
// anything may refuse it: the first is when it was submitted, and the
// billing country of the request (else of the saved card method) is kept.
func (s *Service) MarkSubmitted(ctx context.Context, customerID, id uuid.UUID, facts CheckoutFacts, billingCountry string, method uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	params := gen.MarkCheckoutSubmittedParams{MerchantID: mid.UUID(), CustomerID: customerID, ID: id, Now: s.now(),
		IpCountry: optional(Country(facts.IPCountry)), Language: optional(language(facts.Language)), BillingCountry: optional(Country(billingCountry))}
	if method != uuid.Nil {
		params.PaymentMethodID = &method
	}
	_, err = s.DB.Gen(ctx).MarkCheckoutSubmitted(ctx, params)
	return err
}

// Checkout is what was recorded of the order as a checkout; nil when it is
// not one.
func (o *Order) Checkout() *billing.OrderCheckout {
	if o.PurchaseNumber == nil {
		return nil
	}
	out := &billing.OrderCheckout{PurchaseNumber: int(*o.PurchaseNumber), OpenedAt: o.OpenedAt, SubmittedAt: o.SubmittedAt,
		BillingCountry: o.BillingCountry, CardCountry: o.CardCountry, IPCountry: o.IpCountry, Language: o.Language,
		Attribution: billing.OrderAttribution{Source: o.AttributionSource, Medium: o.AttributionMedium, Campaign: o.AttributionCampaign,
			Referrer: o.AttributionReferrer, Affiliate: o.AttributionAffiliate}}
	for _, evidence := range []struct {
		country *string
		source  billing.CountrySource
	}{{o.BillingCountry, billing.CountrySourceBillingAddress}, {o.CardCountry, billing.CountrySourceCard}, {o.IpCountry, billing.CountrySourceIP}} {
		if evidence.country != nil {
			country, source := *evidence.country, evidence.source
			out.Country, out.CountrySource = &country, &source
			break
		}
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
