package guide

import (
	"context"
	"os"

	"github.com/open-rails/openrails"
)

// Archive writes the merchant's billing archive to a file.
func Archive(ctx context.Context, shop *openrails.Client, path string) error {
	// guide:export-archive
	f, err := os.Create(path) // a JSON Lines stream; every writer must be stopped first
	if err != nil {
		return err
	}
	if err := shop.ExportBillingArchive(ctx, f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
	// guide:end
}
