package orders

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// CheckoutFacts are what a checkout's request tells of its customer: the
// country a Cloudflare proxy reported and the preferred language, both only
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
	hostShape     = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_.-]{0,251}[a-z0-9_])?$`)
	escape        = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
	// cardJoints are separators a card number may be written with beyond
	// the spaces and dashes cardguard reads.
	cardJoints = strings.NewReplacer(".", " ", "/", " ", "_", " ")
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

// AttributionDrop is an attribution field left out of a checkout, and why:
// too_long, unprintable, personal_data (an email address or card number) or
// not_a_host.
type AttributionDrop struct {
	Field  string
	Reason string
}

// CheckAttribution is what of a is kept on a checkout: each field
// percent-decoded, NFKC-normalized and trimmed, the referrer reduced to its
// lowercase ASCII host. A field that is blank is absent; one that is too
// long, unprintable, or carries an email address or a card number is left
// out and reported in drops. Attribution never refuses a sale.
func CheckAttribution(a *billing.OrderAttribution) (kept billing.OrderAttribution, drops []AttributionDrop) {
	if a == nil {
		return billing.OrderAttribution{}, nil
	}
	for _, f := range []struct {
		name string
		in   *string
		out  **string
	}{{"source", a.Source, &kept.Source}, {"medium", a.Medium, &kept.Medium}, {"campaign", a.Campaign, &kept.Campaign}, {"affiliate", a.Affiliate, &kept.Affiliate}} {
		v, reason := attributionText(f.in)
		if reason != "" {
			drops = append(drops, AttributionDrop{Field: f.name, Reason: reason})
		}
		*f.out = v
	}
	referrer, reason := referrerHost(a.Referrer)
	if reason != "" {
		drops = append(drops, AttributionDrop{Field: "referrer", Reason: reason})
	}
	kept.Referrer = referrer
	return kept, drops
}

// decoded is s with its %XX escapes decoded (twice-encoded ones too) and
// NFKC-normalized, so a fullwidth at sign or "%40" reads as "@". A "%" not
// starting an escape stays as written.
func decoded(s string) string {
	for range 3 {
		next := escape.ReplaceAllStringFunc(s, func(e string) string {
			v, err := url.PathUnescape(e)
			if err != nil {
				return e
			}
			return v
		})
		if next == s {
			break
		}
		s = next
	}
	return strings.TrimSpace(norm.NFKC.String(s))
}

func attributionText(in *string) (*string, string) {
	if in == nil {
		return nil, ""
	}
	v := decoded(*in)
	switch {
	case v == "":
		return nil, ""
	case len(v) > maxAttribution:
		return nil, "too_long"
	case !printable(v):
		return nil, "unprintable"
	case strings.Contains(v, "@") || cardguard.ContainsPAN(cardJoints.Replace(v)):
		return nil, "personal_data"
	}
	return &v, ""
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// referrerHost keeps a referrer URL's host, never its path or query.
func referrerHost(in *string) (*string, string) {
	if in == nil || strings.TrimSpace(*in) == "" {
		return nil, ""
	}
	v := strings.TrimSpace(*in)
	if len(v) > 2048 {
		return nil, "too_long"
	}
	host := v
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return nil, "not_a_host"
		}
		host = u.Hostname()
	} else {
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if strings.Count(host, ":") == 1 {
			host, _, _ = strings.Cut(host, ":")
		}
	}
	host, err := idna.Punycode.ToASCII(strings.TrimSuffix(norm.NFKC.String(host), "."))
	host = strings.ToLower(host)
	if err != nil || !hostShape.MatchString(host) {
		return nil, "not_a_host"
	}
	return &host, ""
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

// MarkSubmitted records a pay request on the customer's checkout; see
// MarkCheckoutSubmitted.
func (s *Service) MarkSubmitted(ctx context.Context, customerID, id uuid.UUID, facts CheckoutFacts, billingCountry string, method uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return MarkCheckoutSubmitted(ctx, s.DB, mid.UUID(), customerID, id, facts, billingCountry, method, s.now())
}

// MarkCheckoutSubmitted records a pay request on the customer's open
// checkout, before anything may refuse it: the first is when it was
// submitted, and the billing country of the request (else of the saved card
// method) is kept. A request that would change nothing writes nothing.
func MarkCheckoutSubmitted(ctx context.Context, d *db.DB, merchantID, customerID, id uuid.UUID, facts CheckoutFacts, billingCountry string, method uuid.UUID, now time.Time) error {
	params := gen.MarkCheckoutSubmittedParams{MerchantID: merchantID, CustomerID: customerID, ID: id, Now: now.UTC().Truncate(time.Microsecond),
		IpCountry: optional(Country(facts.IPCountry)), Language: optional(language(facts.Language)), BillingCountry: optional(Country(billingCountry))}
	if method != uuid.Nil {
		params.PaymentMethodID = &method
	}
	_, err := d.Gen(ctx).MarkCheckoutSubmitted(ctx, params)
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
