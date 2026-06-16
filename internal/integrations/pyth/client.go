package pyth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/httpx"
)

const defaultHTTPTimeout = 5 * time.Second

// Config controls Pyth Hermes price lookups.
type Config struct {
	HermesURL        string
	MaxPriceAge      time.Duration
	MaxConfidenceBPS int
	PriceFeeds       map[string]string
	HTTPClient       *http.Client
}

// Price is a validated USD price from Pyth.
type Price struct {
	Symbol        string
	FeedID        string
	USD           float64
	ConfidenceBPS float64
	PublishedAt   time.Time
}

// Client fetches latest prices from Pyth Hermes.
type Client struct {
	hermesURL        string
	maxPriceAge      time.Duration
	maxConfidenceBPS int
	priceFeeds       map[string]string
	httpClient       *http.Client
	now              func() time.Time
}

// NewClient creates a Pyth Hermes client.
func NewClient(cfg Config) (*Client, error) {
	hermesURL := strings.TrimRight(strings.TrimSpace(cfg.HermesURL), "/")
	if hermesURL == "" {
		return nil, fmt.Errorf("pyth hermes URL is required")
	}
	if _, err := url.ParseRequestURI(hermesURL); err != nil {
		return nil, fmt.Errorf("invalid pyth hermes URL: %w", err)
	}
	if cfg.MaxPriceAge <= 0 {
		return nil, fmt.Errorf("pyth max price age must be positive")
	}

	priceFeeds := make(map[string]string, len(cfg.PriceFeeds))
	for symbol, feedID := range cfg.PriceFeeds {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		trimmedFeedID := strings.TrimSpace(feedID)
		if normalizedSymbol == "" || trimmedFeedID == "" {
			continue
		}
		priceFeeds[normalizedSymbol] = trimmedFeedID
	}
	if len(priceFeeds) == 0 {
		return nil, fmt.Errorf("pyth price feed map is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	return &Client{
		hermesURL:        hermesURL,
		maxPriceAge:      cfg.MaxPriceAge,
		maxConfidenceBPS: cfg.MaxConfidenceBPS,
		priceFeeds:       priceFeeds,
		httpClient:       httpClient,
		now:              time.Now,
	}, nil
}

type latestPriceResponse struct {
	Parsed []parsedPrice `json:"parsed"`
}

type parsedPrice struct {
	ID    string `json:"id"`
	Price struct {
		Price       string `json:"price"`
		Conf        string `json:"conf"`
		Expo        int    `json:"expo"`
		PublishTime int64  `json:"publish_time"`
	} `json:"price"`
}

// PriceUSD fetches and validates the latest Pyth USD price for a symbol.
func (c *Client) PriceUSD(ctx context.Context, symbol string) (float64, error) {
	price, err := c.LatestPrice(ctx, symbol)
	if err != nil {
		return 0, err
	}
	return price.USD, nil
}

// LatestPrice fetches and validates the latest Pyth USD price for a symbol.
func (c *Client) LatestPrice(ctx context.Context, symbol string) (Price, error) {
	if c == nil {
		return Price{}, fmt.Errorf("pyth client is nil")
	}
	normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
	if normalizedSymbol == "" {
		return Price{}, fmt.Errorf("token symbol is required")
	}
	feedID := strings.TrimSpace(c.priceFeeds[normalizedSymbol])
	if feedID == "" {
		return Price{}, fmt.Errorf("pyth price feed missing for %s", normalizedSymbol)
	}

	reqURL, err := url.Parse(c.hermesURL + "/v2/updates/price/latest")
	if err != nil {
		return Price{}, fmt.Errorf("parse pyth price URL: %w", err)
	}
	q := reqURL.Query()
	q.Add("ids[]", feedID)
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return Price{}, fmt.Errorf("create pyth price request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Price{}, fmt.Errorf("fetch pyth price for %s: %w", normalizedSymbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Price{}, fmt.Errorf("pyth price for %s returned status %s", normalizedSymbol, resp.Status)
	}

	var decoded latestPriceResponse
	// Cap the upstream body: Pyth Hermes feeds a crypto pricing decision, so an
	// unbounded response must not be read straight into memory.
	if err := httpx.DecodeJSONLimited(resp.Body, 0, &decoded); err != nil {
		return Price{}, fmt.Errorf("decode pyth price for %s: %w", normalizedSymbol, err)
	}

	for _, parsed := range decoded.Parsed {
		if strings.EqualFold(parsed.ID, feedID) {
			return c.validateParsedPrice(normalizedSymbol, feedID, parsed)
		}
	}
	return Price{}, fmt.Errorf("pyth price unavailable for %s", normalizedSymbol)
}

func (c *Client) validateParsedPrice(symbol, feedID string, parsed parsedPrice) (Price, error) {
	priceValue, err := parsePythNumber(parsed.Price.Price, parsed.Price.Expo)
	if err != nil {
		return Price{}, fmt.Errorf("parse pyth price for %s: %w", symbol, err)
	}
	if priceValue <= 0 || math.IsNaN(priceValue) || math.IsInf(priceValue, 0) {
		return Price{}, fmt.Errorf("pyth price for %s is not positive", symbol)
	}

	confValue, err := parsePythNumber(parsed.Price.Conf, parsed.Price.Expo)
	if err != nil {
		return Price{}, fmt.Errorf("parse pyth confidence for %s: %w", symbol, err)
	}
	confidenceBPS := (confValue / priceValue) * 10000
	if c.maxConfidenceBPS > 0 && confidenceBPS > float64(c.maxConfidenceBPS) {
		return Price{}, fmt.Errorf("pyth confidence for %s too wide: %.2f bps", symbol, confidenceBPS)
	}

	publishedAt := time.Unix(parsed.Price.PublishTime, 0).UTC()
	if publishedAt.IsZero() || parsed.Price.PublishTime <= 0 {
		return Price{}, fmt.Errorf("pyth price for %s missing publish time", symbol)
	}
	if age := c.now().Sub(publishedAt); age > c.maxPriceAge {
		return Price{}, fmt.Errorf("pyth price for %s is stale: age %s", symbol, age)
	}

	return Price{
		Symbol:        symbol,
		FeedID:        feedID,
		USD:           priceValue,
		ConfidenceBPS: confidenceBPS,
		PublishedAt:   publishedAt,
	}, nil
}

func parsePythNumber(value string, expo int) (float64, error) {
	mantissa, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, err
	}
	return mantissa * math.Pow10(expo), nil
}
