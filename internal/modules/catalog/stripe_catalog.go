package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/railresolve"
)

type StripeCatalogService struct {
	StripeClients *stripeapi.Factory
	Config        *billing.Config
	// Rails resolves the ctx merchant's armed Stripe account.
	Rails railresolve.Source
	Log   *slog.Logger
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
			s.Log.WarnContext(ctx, "stripe catalog: rail resolution failed; refusing to act", logx.Err(err))
		}
		return nil
	}
	return proc.Stripe
}

// httpClient returns the stripeapi choke-point client: a readonly merchant's
// mutating requests stop at the transport. All Stripe HTTP here goes through it.
func (s *StripeCatalogService) httpClient() *http.Client {
	var clients *stripeapi.Factory
	if s != nil {
		clients = s.StripeClients
	}
	return clients.Client(0)
}

// stripeListPageLimit is Stripe's List page maximum.
const stripeListPageLimit = 100

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
