package main

import (
	"context"
	"fmt"
	"io"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// applyCatalog is the site's catalog command (go run . catalog apply
// catalog.yaml): an operator changes what the site sells, like a deploy. It
// prints what changed and fails when the file skipped an object an edit
// changed; applying the same file again changes nothing.
func applyCatalog(ctx context.Context, shop *openrails.Client, path string, out io.Writer) error {
	products, err := catalog.ReadFile(path)
	if err != nil {
		return err
	}
	receipt, err := shop.ApplyCatalog(ctx, products, billing.ApplyCatalogParams{})
	if err != nil {
		return err
	}
	if err := openrails.WriteCatalogReceipt(out, receipt); err != nil {
		return err
	}
	if n := len(receipt.Conflicts); n > 0 {
		return fmt.Errorf("%s skipped %d object(s) an edit changed", path, n)
	}
	return nil
}
