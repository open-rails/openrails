package server

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
	"net/http"
)

// NotificationEvents binds the host's independent global notification consumer.
func (s *Server) NotificationEvents(name string) (*openrails.NotificationConsumer, error) {
	return s.engine.NotificationEvents(name)
}

// NetworkAccountHandler admits only this server's native self-acting users who
// accepted network terms. The hosted application owns next and its inbox API.
func (s *Server) NetworkAccountHandler(next func(http.ResponseWriter, *http.Request, billing.NetworkAccountID)) http.Handler {
	return routes.NetworkAccountHandler(s.graph.Runtime, s.AuthKit().Authenticator(), next)
}

// NetworkLinks returns the account's currently active explicit customer links.
// Call from an admitted account handler; this also checks accepted terms.
func (s *Server) NetworkLinks(ctx context.Context, account billing.NetworkAccountID) ([]billing.CustomerLink, error) {
	if s.network == nil {
		return nil, fmt.Errorf("customer network is unavailable")
	}
	if err := s.network.Admit(ctx, account); err != nil {
		return nil, err
	}
	return s.network.Links(ctx, account)
}
