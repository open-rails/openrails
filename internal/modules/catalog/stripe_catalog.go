package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"

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

// CreateProductParams carries the inputs for creating a Stripe Product owned by OpenRails.
type CreateProductParams struct {
	Name           string
	Description    string
	IdempotencyKey string
	// Metadata is written to the Stripe Product; discovery searches
	// StripeMetadataOpenRailsProductKey.
	Metadata map[string]string
}

// CreateProduct creates a Stripe Product. The metadata is merged into the request
// so reruns of an idempotent caller can be located via metadata search.
func (s *StripeCatalogService) CreateProduct(ctx context.Context, params CreateProductParams) (string, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return "", fmt.Errorf("stripe product name required")
	}

	form := url.Values{}
	form.Set("name", name)
	if strings.TrimSpace(params.Description) != "" {
		form.Set("description", params.Description)
	}
	for k, v := range params.Metadata {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		form.Set("metadata["+k+"]", v)
	}

	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/products", form, params.IdempotencyKey)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

// CreatePriceParams carries the inputs for creating a Stripe Price owned by OpenRails.
type CreatePriceParams struct {
	StripeProductID  string
	UnitAmount       int64
	Currency         string
	BillingCycleDays *int
	// LookupKey is set on the Stripe Price so it can be retrieved without
	// knowing the remote ID: "openrails.<local-price-uuid>".
	LookupKey      string
	Metadata       map[string]string
	IdempotencyKey string
}

// CreatePrice creates a Stripe Price with lookup_key and metadata so reruns and
// reconciliation paths can find the object without retaining its Stripe ID.
func (s *StripeCatalogService) CreatePrice(ctx context.Context, params CreatePriceParams) (string, error) {
	if err := moneyutil.RequireFiatCurrency(params.Currency); err != nil {
		return "", err
	}
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	productID := strings.TrimSpace(params.StripeProductID)
	if productID == "" {
		return "", fmt.Errorf("stripe_product_id required")
	}
	if params.UnitAmount <= 0 {
		return "", fmt.Errorf("unit_amount must be positive")
	}
	currency := strings.ToLower(strings.TrimSpace(params.Currency))
	if currency == "" {
		return "", fmt.Errorf("currency required")
	}

	form := url.Values{}
	form.Set("product", productID)
	form.Set("unit_amount", strconv.FormatInt(params.UnitAmount, 10))
	form.Set("currency", currency)
	if lk := strings.TrimSpace(params.LookupKey); lk != "" {
		form.Set("lookup_key", lk)
		// Make this lookup_key authoritative: transfer ownership away from any prior price.
		form.Set("transfer_lookup_key", "true")
	}
	for k, v := range params.Metadata {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		form.Set("metadata["+k+"]", v)
	}

	// recurring price for subscriptions
	if params.BillingCycleDays != nil && *params.BillingCycleDays > 0 {
		interval, intervalCount := stripeIntervalForDays(*params.BillingCycleDays)
		form.Set("recurring[interval]", interval)
		if intervalCount > 1 {
			form.Set("recurring[interval_count]", strconv.Itoa(intervalCount))
		}
	}

	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/prices", form, params.IdempotencyKey)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

// UpdateProductParams: mutable Stripe Product fields. Nil means "leave unchanged."
type UpdateProductParams struct {
	Name        *string
	Description *string
	Active      *bool
	// Metadata replaces the keys provided; to clear a key set its value to "".
	Metadata map[string]string
	// IdempotencyKey, when set, is sent as the Stripe Idempotency-Key header so
	// Stripe deduplicates a replayed mutation (e.g. a reclaimed archive intent).
	IdempotencyKey string
}

// UpdateProduct propagates mutable fields to an existing Stripe Product.
func (s *StripeCatalogService) UpdateProduct(ctx context.Context, stripeProductID string, params UpdateProductParams) error {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripeProductID)
	if id == "" {
		return fmt.Errorf("stripe_product_id required")
	}
	form := url.Values{}
	if params.Name != nil {
		form.Set("name", strings.TrimSpace(*params.Name))
	}
	if params.Description != nil {
		form.Set("description", strings.TrimSpace(*params.Description))
	}
	if params.Active != nil {
		form.Set("active", strconv.FormatBool(*params.Active))
	}
	for k, v := range params.Metadata {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		form.Set("metadata["+k+"]", v)
	}
	if len(form) == 0 {
		return nil
	}
	_, err := s.stripePostForm(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/products/"+url.PathEscape(id), form, params.IdempotencyKey)
	return err
}

// UpdatePriceParams: mutable Stripe Price fields. Stripe disallows mutating
// unit_amount/currency/interval after create — those rules live in OpenRails too.
type UpdatePriceParams struct {
	Nickname  *string // free-text display name
	Active    *bool
	LookupKey *string // set to "" to clear
	Metadata  map[string]string
	// IdempotencyKey: see UpdateProductParams.IdempotencyKey.
	IdempotencyKey string
}

// UpdatePrice propagates mutable fields to an existing Stripe Price.
func (s *StripeCatalogService) UpdatePrice(ctx context.Context, stripePriceID string, params UpdatePriceParams) error {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripePriceID)
	if id == "" {
		return fmt.Errorf("stripe_price_id required")
	}
	form := url.Values{}
	if params.Nickname != nil {
		form.Set("nickname", strings.TrimSpace(*params.Nickname))
	}
	if params.Active != nil {
		form.Set("active", strconv.FormatBool(*params.Active))
	}
	if params.LookupKey != nil {
		form.Set("lookup_key", strings.TrimSpace(*params.LookupKey))
		form.Set("transfer_lookup_key", "true")
	}
	for k, v := range params.Metadata {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		form.Set("metadata["+k+"]", v)
	}
	if len(form) == 0 {
		return nil
	}
	_, err := s.stripePostForm(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/prices/"+url.PathEscape(id), form, params.IdempotencyKey)
	return err
}

// StripeProduct is the subset of Stripe's Product resource OpenRails reads.
type StripeProduct struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Active      bool              `json:"active"`
	Metadata    map[string]string `json:"metadata"`
}

// RetrieveProduct fetches a Stripe Product by ID; a 404 is a "not found"
// error. FindProduct reports absence without an error.
func (s *StripeCatalogService) RetrieveProduct(ctx context.Context, stripeProductID string) (*StripeProduct, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripeProductID)
	if id == "" {
		return nil, fmt.Errorf("stripe_product_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/products/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("stripe product %s not found", id)
	}
	if status >= 300 {
		return nil, fmt.Errorf("stripe product retrieve failed: %s", parseStripeError(body))
	}
	var out StripeProduct
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FindProduct fetches a Stripe Product by ID. A 404 returns (nil, false, nil),
// so verify-then-execute callers tell "gone" from "read failed".
func (s *StripeCatalogService) FindProduct(ctx context.Context, stripeProductID string) (*StripeProduct, bool, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripeProductID)
	if id == "" {
		return nil, false, fmt.Errorf("stripe_product_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/products/"+url.PathEscape(id))
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if status >= 300 {
		return nil, false, fmt.Errorf("stripe product retrieve failed: %s", parseStripeError(body))
	}
	var out StripeProduct
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
}

// FindPrice fetches a Stripe Price by ID; 404 returns (nil, false, nil). See
// FindProduct.
func (s *StripeCatalogService) FindPrice(ctx context.Context, stripePriceID string) (*StripePrice, bool, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripePriceID)
	if id == "" {
		return nil, false, fmt.Errorf("stripe_price_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/prices/"+url.PathEscape(id))
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if status >= 300 {
		return nil, false, fmt.Errorf("stripe price retrieve failed: %s", parseStripeError(body))
	}
	var out StripePrice
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
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

// SearchProductsByMetadata returns up to 10 Stripe Products whose metadata key
// equals value. Stripe Search is eventually consistent (~30-60s lag).
func (s *StripeCatalogService) SearchProductsByMetadata(ctx context.Context, key, value string) ([]StripeProduct, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return nil, fmt.Errorf("metadata key and value required")
	}
	// Stripe search query syntax: metadata['key']:'value'
	query := fmt.Sprintf("metadata['%s']:'%s'", escapeStripeQueryValue(key), escapeStripeQueryValue(value))
	endpoint := s.baseURL() + "/v1/products/search?limit=10&query=" + url.QueryEscape(query)
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("stripe product search failed: %s", parseStripeError(body))
	}
	var resp struct {
		Data []StripeProduct `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// ListPricesByLookupKey returns the Stripe Prices with lookupKey through the
// strongly consistent List API. More than one means duplicate creation.
func (s *StripeCatalogService) ListPricesByLookupKey(ctx context.Context, lookupKey string) ([]StripePrice, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	lookupKey = strings.TrimSpace(lookupKey)
	if lookupKey == "" {
		return nil, fmt.Errorf("lookup_key required")
	}
	endpoint := s.baseURL() + "/v1/prices?limit=10&lookup_keys[]=" + url.QueryEscape(lookupKey)
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("stripe price list failed: %s", parseStripeError(body))
	}
	var resp struct {
		Data []StripePrice `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func escapeStripeQueryValue(s string) string {
	// Stripe search syntax requires escaping single quotes in literal values.
	return strings.ReplaceAll(s, "'", "\\'")
}

// stripeListPageLimit is Stripe's List page maximum.
const stripeListPageLimit = 100

// ListProducts returns one page of Stripe Products. Pass "" for the first page,
// then the returned nextCursor; it is "" when no pages remain.
func (s *StripeCatalogService) ListProducts(ctx context.Context, startingAfter string) (products []StripeProduct, nextCursor string, err error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, "", fmt.Errorf("stripe is not configured")
	}
	endpoint := fmt.Sprintf(s.baseURL()+"/v1/products?limit=%d", stripeListPageLimit)
	if sa := strings.TrimSpace(startingAfter); sa != "" {
		endpoint += "&starting_after=" + url.QueryEscape(sa)
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
	if err != nil {
		return nil, "", err
	}
	if status >= 300 {
		return nil, "", fmt.Errorf("stripe product list failed: %s", parseStripeError(body))
	}
	var resp struct {
		Data    []StripeProduct `json:"data"`
		HasMore bool            `json:"has_more"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", err
	}
	if resp.HasMore && len(resp.Data) > 0 {
		nextCursor = resp.Data[len(resp.Data)-1].ID
	}
	return resp.Data, nextCursor, nil
}

// ListPrices returns one page of Stripe Prices, paged like ListProducts.
func (s *StripeCatalogService) ListPrices(ctx context.Context, startingAfter string) (prices []StripePrice, nextCursor string, err error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, "", fmt.Errorf("stripe is not configured")
	}
	endpoint := fmt.Sprintf(s.baseURL()+"/v1/prices?limit=%d", stripeListPageLimit)
	if sa := strings.TrimSpace(startingAfter); sa != "" {
		endpoint += "&starting_after=" + url.QueryEscape(sa)
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
	if err != nil {
		return nil, "", err
	}
	if status >= 300 {
		return nil, "", fmt.Errorf("stripe price list failed: %s", parseStripeError(body))
	}
	var resp struct {
		Data    []StripePrice `json:"data"`
		HasMore bool          `json:"has_more"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", err
	}
	if resp.HasMore && len(resp.Data) > 0 {
		nextCursor = resp.Data[len(resp.Data)-1].ID
	}
	return resp.Data, nextCursor, nil
}

// RetrievePrice fetches a Stripe Price by ID.
func (s *StripeCatalogService) RetrievePrice(ctx context.Context, stripePriceID string) (*StripePrice, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripePriceID)
	if id == "" {
		return nil, fmt.Errorf("stripe_price_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/prices/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("stripe price %s not found", id)
	}
	if status >= 300 {
		return nil, fmt.Errorf("stripe price retrieve failed: %s", parseStripeError(body))
	}
	var out StripePrice
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

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
