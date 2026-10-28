package providerobjects

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// decimal is a plain non-negative major-unit amount; currencyCode the codes
// the observed catalog stores.
var (
	decimal      = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	currencyCode = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,9}$`)
)

type nmiReader struct{ client *nmi.NMIClient }

func (r nmiReader) List(ctx context.Context) ([]Object, error) {
	plans, err := r.client.ListRecurringPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("nmi plans: %w", err)
	}
	out := make([]Object, 0, len(plans))
	for _, p := range plans {
		if obj := NMIPlan(p); obj.Ref != "" {
			out = append(out, obj)
		}
	}
	return out, nil
}

func (r nmiReader) Get(ctx context.Context, kind Kind, ref string) (Object, bool, error) {
	if kind != Price {
		return Object{}, false, nil
	}
	plan, found, err := r.client.GetRecurringPlan(ctx, ref)
	if err != nil || !found {
		return Object{}, found, err
	}
	obj := NMIPlan(plan)
	if obj.Ref == "" {
		obj.Ref = ref
	}
	return obj, true, nil
}

// NMIPlan is an NMI plan as a price: NMI keeps no active flag and names no
// currency. A plan stating both frequencies states no interval.
func NMIPlan(p nmi.V5Plan) Object {
	obj := Object{Kind: Price, Ref: strings.TrimSpace(p.ID), Name: strings.TrimSpace(p.PlanName), Active: true}
	if amount := strings.TrimSpace(p.PlanAmount); decimal.MatchString(amount) {
		obj.Amount = amount
	}
	days, _ := strconv.Atoi(strings.TrimSpace(p.DayFrequency))
	months, _ := strconv.Atoi(strings.TrimSpace(p.MonthFrequency))
	switch {
	case days > 0 && months > 0:
	case days > 0:
		obj.IntervalHours = days * 24
	case months > 0:
		obj.IntervalMonths = months
	}
	obj.Evidence, _ = json.Marshal(p)
	return obj
}

type stripeReader struct{ fetcher *reconcile.StripeFetcher }

func newStripeReader(secretKey string, client *http.Client) stripeReader {
	fetcher := reconcile.NewStripeFetcher(secretKey)
	fetcher.HTTPClient = client
	return stripeReader{fetcher: fetcher}
}

// List reads products and prices, active ones first, then inactive ones:
// Stripe lists only active prices unless asked. Prices created inline on a
// subscription are never listed; a by-id read observes them. An object it
// cannot read is skipped, not the catalog.
func (r stripeReader) List(ctx context.Context) ([]Object, error) {
	var out []Object
	for _, path := range []string{"/v1/products", "/v1/prices"} {
		for _, active := range []string{"true", "false"} {
			raws, err := r.fetcher.StripeList(ctx, path, time.Time{}, time.Time{}, "", url.Values{"active": {active}})
			if err != nil {
				return nil, fmt.Errorf("stripe %s: %w", strings.TrimPrefix(path, "/v1/"), err)
			}
			for _, raw := range raws {
				obj, ok, err := stripeObject(raw)
				if err != nil {
					log.WithContext(ctx).WithError(err).Warn("provider catalog: skipped an unreadable Stripe object")
					continue
				}
				if ok {
					out = append(out, obj)
				}
			}
		}
	}
	return out, nil
}

func (r stripeReader) Get(ctx context.Context, kind Kind, ref string) (Object, bool, error) {
	path := "/v1/prices/"
	if kind == Product {
		path = "/v1/products/"
	}
	raw, found, err := r.fetcher.StripeGet(ctx, path+url.PathEscape(ref))
	if err != nil || !found {
		return Object{}, false, err
	}
	return stripeObject(raw)
}

type stripeJSON struct {
	Object     string          `json:"object"`
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Nickname   string          `json:"nickname"`
	Active     bool            `json:"active"`
	Deleted    bool            `json:"deleted"`
	Product    json.RawMessage `json:"product"`
	Currency   string          `json:"currency"`
	UnitAmount *int64          `json:"unit_amount"`
	Recurring  *struct {
		Interval      string `json:"interval"`
		IntervalCount int    `json:"interval_count"`
	} `json:"recurring"`
}

// stripeObject parses a product or price; ok is false for a deleted product
// or another object.
func stripeObject(raw json.RawMessage) (Object, bool, error) {
	var s stripeJSON
	if err := json.Unmarshal(raw, &s); err != nil || s.ID == "" {
		return Object{}, false, fmt.Errorf("stripe catalog object is unreadable")
	}
	obj := Object{Ref: s.ID, Active: s.Active, Evidence: raw}
	switch s.Object {
	case "product":
		if s.Deleted {
			return Object{}, false, nil
		}
		obj.Kind, obj.Name = Product, strings.TrimSpace(s.Name)
	case "price":
		obj.Kind, obj.Name = Price, strings.TrimSpace(s.Nickname)
		obj.ProductRef = stripeRef(s.Product)
		if code := moneyutil.NormalizeCurrency(s.Currency); currencyCode.MatchString(code) {
			obj.Currency = code
		}
		// A currency OpenRails does not hold leaves no amount: mapping the
		// price is refused, the rest of the catalog is not.
		if s.UnitAmount != nil {
			if amount, err := majorUnits(*s.UnitAmount, obj.Currency); err == nil {
				obj.Amount = amount
			}
		}
		if s.Recurring != nil && s.Recurring.IntervalCount > 0 {
			switch n := s.Recurring.IntervalCount; s.Recurring.Interval {
			case "day":
				obj.IntervalHours = n * 24
			case "week":
				obj.IntervalHours = n * 7 * 24
			case "month":
				obj.IntervalMonths = n
			case "year":
				obj.IntervalMonths = n * 12
			}
		}
	default:
		return Object{}, false, nil
	}
	return obj, true, nil
}

// stripeRef reads an expandable field: an id, or an object with one.
func stripeRef(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var obj struct{ ID string }
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}

// majorUnits writes a minor-unit amount (cents) as its decimal major units.
func majorUnits(minor int64, currency string) (string, error) {
	cur, ok := moneyutil.LookupCurrency(currency)
	if !ok {
		return "", fmt.Errorf("unknown currency %q", currency)
	}
	if minor < 0 {
		return "", fmt.Errorf("negative amount %d", minor)
	}
	digits := strconv.FormatInt(minor, 10)
	if cur.MinorDecimals == 0 {
		return digits, nil
	}
	if pad := cur.MinorDecimals + 1 - len(digits); pad > 0 {
		digits = strings.Repeat("0", pad) + digits
	}
	cut := len(digits) - cur.MinorDecimals
	return digits[:cut] + "." + digits[cut:], nil
}

// Stated is a provider price's last terms as OpenRails stores them (amount
// in native units): what a read finding its object gone records, when the
// object was never observed. namesCurrency is false for NMI.
func Stated(ref string, amount int64, currency string, namesCurrency bool, hours, months *int) *Object {
	obj := &Object{Kind: Price, Ref: ref, Active: true, Evidence: json.RawMessage(`{"source":"openrails_provider_price"}`)}
	if minor, err := moneyutil.NativeToRailMinorExact(currency, amount); err == nil {
		if major, err := majorUnits(int64(minor), currency); err == nil {
			obj.Amount = major
		}
	}
	if code := moneyutil.NormalizeCurrency(currency); namesCurrency && currencyCode.MatchString(code) {
		obj.Currency = code
	}
	switch {
	case months != nil && *months > 0:
		obj.IntervalMonths = *months
	case hours != nil && *hours > 0:
		obj.IntervalHours = *hours
	}
	return obj
}
