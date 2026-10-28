package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"

	log "github.com/sirupsen/logrus"
)

type StripeCatalogService struct {
	StripeClients *stripeapi.Factory
	Config        *config.Config
	// Rails resolves the ctx merchant's armed Stripe account.
	Rails railresolve.Source
	// BaseURL overrides the Stripe API root; tests point it at an httptest
	// server.
	BaseURL string
}

// baseURL returns the Stripe API root, honoring the test override.
func (s *StripeCatalogService) baseURL() string {
	if s != nil && strings.TrimSpace(s.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	}
	return "https://api.stripe.com"
}

// stripeRail resolves the ctx merchant's armed Stripe credentials. nil (not
// armed, or a logged resolution error) makes every caller refuse; it never
// falls back to another account.
func (s *StripeCatalogService) stripeRail(ctx context.Context) *config.StripeRailConfig {
	if s == nil || s.Rails == nil {
		return nil
	}
	proc, err := s.Rails.RailConfig(ctx, string(models.RailStripe), "")
	if err != nil {
		if !errors.Is(err, railresolve.ErrRailNotArmed) {
			log.WithContext(ctx).WithError(err).Warn("stripe catalog: rail resolution failed; refusing to act")
		}
		return nil
	}
	return proc.Stripe
}

// httpClient returns the stripeapi choke-point client: readonly mode rejects
// mutating requests at the transport. All Stripe HTTP here goes through it.
func (s *StripeCatalogService) httpClient() *http.Client {
	var cfg *config.Config
	if s != nil {
		cfg = s.Config
	}
	var clients *stripeapi.Factory
	if s != nil {
		clients = s.StripeClients
	}
	return clients.Client(cfg, 0)
}

type stripeObject struct {
	ID string `json:"id"`
}

// Stripe metadata keys marking catalog objects OpenRails owns. Products match
// their declared keys, prices their retained local IDs; stored bindings take
// precedence over metadata discovery.
const (
	// StripeMetadataOpenRailsProductKey holds the OpenRails product key.
	// AutoCreate searches on it, so it must survive DB wipes.
	StripeMetadataOpenRailsProductKey = "openrails_product_key"
	// StripeMetadataOpenRailsPriceKey holds the immutable local price UUID.
	// Older objects may retain their historical financial-content marker.
	StripeMetadataOpenRailsPriceKey = "openrails_price_key"

	// StripeMetadataOpenRailsProductID / ...PriceID retain local identities.
	// Price IDs distinguish sibling keys and immutable revisions during discovery.
	StripeMetadataOpenRailsProductID = "openrails_product_id"
	StripeMetadataOpenRailsPriceID   = "openrails_price_id"

	// Recovery envelope: a format version and a hash of the product's
	// entitlements, which providers do not own.
	StripeMetadataOpenRailsRecoveryVersion    = "openrails_recovery_version"
	StripeMetadataOpenRailsBenefitFingerprint = "openrails_benefit_fingerprint"
)

// StripeProduct is the subset of Stripe's Product resource OpenRails reads.
type StripeProduct struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Active      bool              `json:"active"`
	Metadata    map[string]string `json:"metadata"`
}

// StripePrice is the subset of Stripe's Price resource OpenRails reads.
type StripePrice struct {
	ID         string            `json:"id"`
	Product    string            `json:"product"`
	UnitAmount int64             `json:"unit_amount"`
	Currency   string            `json:"currency"`
	Nickname   string            `json:"nickname"`
	LookupKey  string            `json:"lookup_key"`
	Active     bool              `json:"active"`
	Metadata   map[string]string `json:"metadata"`
	Recurring  *struct {
		Interval string `json:"interval"`
		Count    int    `json:"interval_count"`
	} `json:"recurring,omitempty"`
}

// stripeListPageLimit is Stripe's List page maximum.
const stripeListPageLimit = 100

// StripeIntervalForDays exposes the OpenRails-billing-cycle -> Stripe recurrence
// mapping so callers outside this package can validate linked Stripe Prices.
func StripeIntervalForDays(days int) (interval string, intervalCount int) {
	return stripeIntervalForDays(days)
}

func stripeIntervalForDays(days int) (interval string, intervalCount int) {
	if days <= 0 {
		return "month", 1
	}
	switch days {
	case 7:
		return "week", 1
	case 30:
		return "month", 1
	case 365:
		return "year", 1
	default:
		return "day", days
	}
}

// stripeGet executes a GET against the Stripe API and returns the body, status, and any transport error.
// Status >= 400 is not an error; callers inspect status for 404 vs other failures.
func (s *StripeCatalogService) stripeGet(ctx context.Context, secretKey, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// stripeDelete executes a DELETE against the Stripe API and returns the body and
// status. Status >= 400 is not an error; callers inspect status (404 is benign
// for "already gone" detach paths).
func (s *StripeCatalogService) stripeDelete(ctx context.Context, secretKey, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (s *StripeCatalogService) stripePostForm(ctx context.Context, secretKey string, endpoint string, form url.Values, idempotencyKey string) (*stripeObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if strings.TrimSpace(idempotencyKey) != "" {
		req.Header.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey))
	}

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe error: %s", parseStripeError(body))
	}
	var out stripeObject
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("stripe response missing id")
	}
	return &out, nil
}

func parseStripeError(body []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Error.Message)
}
