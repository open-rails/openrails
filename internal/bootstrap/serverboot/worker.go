package serverboot

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	embcp "github.com/open-rails/openrails/internal/operator"
)

// NewWorker contributes the same AuthKit jobs as a standalone API
// process. It constructs no listener and still leaves worker startup to its caller.
func NewWorker(ctx context.Context, cfg *config.Config) (*app.App, error) {
	application, err := app.Bootstrap(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		_ = application.Close(context.Background())
		return nil, fmt.Errorf("attach worker control plane: %w", err)
	}
	return application, nil
}
