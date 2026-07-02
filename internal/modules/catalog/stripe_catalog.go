package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

type StripeCatalogService struct {
	Config *config.Config
	Rails  config.RailMerchantAccountSet
	// BaseURL overrides the Stripe API root (https://api.stripe.com). Empty in
	// production; tests point it at an httptest server. Only the entitlements
	// (Features) methods read it via baseURL(); the older methods hardcode the
	// real host.
	BaseURL string
}

// baseURL returns the Stripe API root, honoring the test override.
func (s *StripeCatalogService) baseURL() string {
	if s != nil && strings.TrimSpace(s.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	}
	return "https://api.stripe.com"
}

func (s *StripeCatalogService) stripeRail() *config.StripeRailConfig {
	if s == nil {
		return nil
	}
	if rail := s.Rails.GetStripeRail(); rail != nil {
		return rail.Stripe
	}
	return nil
}

// httpClient returns the choke-point Stripe client (internal/integrations/
// stripeapi): when mode=readonly every mutating request is rejected at the
// transport with stripeapi.ErrProviderReadOnly before reaching the network.
// ALL Stripe HTTP in this package must flow through it.
func (s *StripeCatalogService) httpClient() *http.Client {
	var cfg *config.Config
	if s != nil {
		cfg = s.Config
	}
	return stripeapi.Client(cfg, 0)
}

type stripeObject struct {
	ID string `json:"id"`
}

// Stripe metadata keys used by OpenRails to mark catalog items it owns.
// These are read on both reconciliation and orphan-discovery paths.
//
// Identity / matching keys are CONTENT-based (derived from catalog product keys) so
// they survive a DB rebuild (which regenerates row UUIDs). The *ID keys are
// retained as informational breadcrumbs only — never used for matching.
const (
	// StripeMetadataOpenRailsProductKey is the content key stamped on a Stripe
	// Product: the OpenRails product key. This is the field SEARCHED on to
	// find-or-create the Stripe Product, so it must be stable across DB wipes.
	StripeMetadataOpenRailsProductKey = "openrails_product_key"
	// StripeMetadataOpenRailsPriceKey is the content key stamped on a Stripe
	// Price for symmetry: "<product_key>.<price_terms>".
	StripeMetadataOpenRailsPriceKey = "openrails_price_key"

	// StripeMetadataOpenRailsProductID / ...PriceID carry the OpenRails row
	// UUIDs as informational breadcrumbs. They change on every DB rebuild and
	// are NOT used for identity or matching.
	StripeMetadataOpenRailsProductID = "openrails_product_id"
	StripeMetadataOpenRailsPriceID   = "openrails_price_id"

	// Recovery envelope keys (#596). Product/price keys are stable catalog
	// identity; BenefitFingerprint snapshots the OpenRails-only benefits that
	// providers do not own, so pull-provider can tell what local grant semantics
	// to materialize after a DB rebuild/import.
	StripeMetadataOpenRailsRecoveryVersion    = "openrails_recovery_version"
	StripeMetadataOpenRailsBenefitFingerprint = "openrails_benefit_fingerprint"
)

// CreateProductParams carries the inputs for creating a Stripe Product owned by OpenRails.
type CreateProductParams struct {
	Name           string
	Description    string
	IdempotencyKey string
	// Metadata is written to the Stripe Product. OpenRails always includes
	// StripeMetadataOpenRailsProductID so the object can be discovered later.
	Metadata map[string]string
}

// CreateProduct creates a Stripe Product. The metadata is merged into the request
// so reruns of an idempotent caller can be located via metadata search.
func (s *StripeCatalogService) CreateProduct(ctx context.Context, params CreateProductParams) (string, error) {
	stripeProc := s.stripeRail()
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

	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/products", form, params.IdempotencyKey)
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
	// knowing the ID. Format is deterministic per OpenRails:
	// "<app_namespace>.<tier_group>.<product>.<price>.<currency>.<amount>.<interval>.<count>".
	LookupKey      string
	Metadata       map[string]string
	IdempotencyKey string
}

// CreatePrice creates a Stripe Price with lookup_key and metadata so reruns and
// reconciliation paths can find the object without retaining its Stripe ID.
func (s *StripeCatalogService) CreatePrice(ctx context.Context, params CreatePriceParams) (string, error) {
	stripeProc := s.stripeRail()
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

	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/prices", form, params.IdempotencyKey)
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
	// a replayed mutation (e.g. an archive intent's lease reclaim, #358) is
	// deduplicated by Stripe itself.
	IdempotencyKey string
}

// UpdateProduct propagates mutable fields to an existing Stripe Product.
func (s *StripeCatalogService) UpdateProduct(ctx context.Context, stripeProductID string, params UpdateProductParams) error {
	stripeProc := s.stripeRail()
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
	_, err := s.stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/products/"+url.PathEscape(id), form, params.IdempotencyKey)
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
	stripeProc := s.stripeRail()
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
	_, err := s.stripePostForm(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/prices/"+url.PathEscape(id), form, params.IdempotencyKey)
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

// RetrieveProduct fetches a Stripe Product by ID. Returns os.ErrNotExist
// (wrapped as fmt error containing "not found") when the ID 404s, so callers
// can distinguish drift from missing.
func (s *StripeCatalogService) RetrieveProduct(ctx context.Context, stripeProductID string) (*StripeProduct, error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripeProductID)
	if id == "" {
		return nil, fmt.Errorf("stripe_product_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/products/"+url.PathEscape(id))
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

// FindProduct fetches a Stripe Product by ID, with absence as a first-class
// answer: a 404 returns (nil, false, nil) so verify-then-execute callers (#358
// archive intents) can distinguish "gone" from "read failed".
func (s *StripeCatalogService) FindProduct(ctx context.Context, stripeProductID string) (*StripeProduct, bool, error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripeProductID)
	if id == "" {
		return nil, false, fmt.Errorf("stripe_product_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/products/"+url.PathEscape(id))
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
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, false, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripePriceID)
	if id == "" {
		return nil, false, fmt.Errorf("stripe_price_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/prices/"+url.PathEscape(id))
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

// SearchProductsByMetadata returns Stripe Products whose metadata key/value
// matches the given pair. Uses Stripe's Search API, which is eventually
// consistent (~30-60s lag). Returns at most 10 matches; the typical OpenRails
// expectation is 0 or 1.
//
// Use this when the OpenRails DB has lost track of a Stripe Product (or to
// detect operator-pre-created products). For normal create flows, the Stripe
// ID stored on the OpenRails row is the primary lookup.
func (s *StripeCatalogService) SearchProductsByMetadata(ctx context.Context, key, value string) ([]StripeProduct, error) {
	stripeProc := s.stripeRail()
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
	endpoint := "https://api.stripe.com/v1/products/search?limit=10&query=" + url.QueryEscape(query)
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

// ListPricesByLookupKey returns Stripe Prices with the given lookup_key.
// Uses Stripe's List API (strongly consistent), unlike SearchProductsByMetadata.
// Lookup keys are unique across active prices in an account, so 0 or 1 result
// is typical; multiple results indicate prior duplicate creation that should
// be cleaned up.
func (s *StripeCatalogService) ListPricesByLookupKey(ctx context.Context, lookupKey string) ([]StripePrice, error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	lookupKey = strings.TrimSpace(lookupKey)
	if lookupKey == "" {
		return nil, fmt.Errorf("lookup_key required")
	}
	endpoint := "https://api.stripe.com/v1/prices?limit=10&lookup_keys[]=" + url.QueryEscape(lookupKey)
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

// stripeListPageLimit is the page size for the catalog reconciliation List calls.
// Stripe caps List endpoints at 100 per page; the reconciliation loop paginates
// with starting_after until has_more is false.
const stripeListPageLimit = 100

// ListProducts returns one page of Stripe Products using the List API. Pass
// startingAfter="" for the first page; on subsequent calls pass the nextCursor
// returned by the prior call. nextCursor is "" when there are no more pages
// (Stripe's has_more=false). Used by the catalog reconciliation loop to
// enumerate the full catalog and detect orphans / drift.
func (s *StripeCatalogService) ListProducts(ctx context.Context, startingAfter string) (products []StripeProduct, nextCursor string, err error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, "", fmt.Errorf("stripe is not configured")
	}
	endpoint := fmt.Sprintf("https://api.stripe.com/v1/products?limit=%d", stripeListPageLimit)
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

// ListPrices returns one page of Stripe Prices using the List API. Pagination
// semantics match ListProducts: pass startingAfter="" for the first page and
// the returned nextCursor for subsequent pages; nextCursor is "" when exhausted.
func (s *StripeCatalogService) ListPrices(ctx context.Context, startingAfter string) (prices []StripePrice, nextCursor string, err error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, "", fmt.Errorf("stripe is not configured")
	}
	endpoint := fmt.Sprintf("https://api.stripe.com/v1/prices?limit=%d", stripeListPageLimit)
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
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	id := strings.TrimSpace(stripePriceID)
	if id == "" {
		return nil, fmt.Errorf("stripe_price_id required")
	}
	body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, "https://api.stripe.com/v1/prices/"+url.PathEscape(id))
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

func (s *StripeCatalogService) VerifyPriceExists(ctx context.Context, priceID string) error {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	priceID = strings.TrimSpace(priceID)
	if priceID == "" {
		return fmt.Errorf("stripe price_id required")
	}

	endpoint := "https://api.stripe.com/v1/prices/" + url.PathEscape(priceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+stripeProc.SecretKey)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("stripe price verification failed: %s", parseStripeError(body))
	}
	return nil
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
