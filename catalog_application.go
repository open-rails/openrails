package openrails

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/service"
)

// ApplyCatalog applies a catalog document as the apply manager. Each product,
// price key and meter in it applies whole, unless a field it names was set
// differently by an edit: that object is skipped and listed in the receipt's
// Conflicts, the rest applies, and the document is not recorded as applied,
// so applying it again retries the skipped objects. params.Force overwrites
// those fields instead. A document that applied whole is remembered by its
// canonical content hash: applying it again returns the original receipt even
// after later catalog edits. It is an operator action, never a side effect
// of starting: run it from a command (openrails catalog apply, or the host's
// own) or the console.
func (c *Client) ApplyCatalog(ctx context.Context, document *catalog.Application, params billing.ApplyCatalogParams, requestOptions ...RequestOption) (*billing.CatalogApplicationReceipt, error) {
	if document == nil {
		return nil, invalidErr("catalog document is required")
	}
	if err := document.Validate(); err != nil {
		return nil, invalidErr(err.Error())
	}
	path := "/v1/catalog/apply"
	if params.Force {
		path = withQuery(path, url.Values{"force": {"true"}})
	}
	var out billing.CatalogApplicationReceipt
	if err := c.do(ctx, http.MethodPost, path, document, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// WriteCatalogReceipt writes what an ApplyCatalog changed and what it skipped,
// for a person: a host's own catalog command prints it as openrails catalog
// apply does.
func WriteCatalogReceipt(w io.Writer, r *billing.CatalogApplicationReceipt) error {
	if r == nil {
		return invalidErr("catalog receipt is required")
	}
	var b strings.Builder
	if r.Replayed {
		fmt.Fprintf(&b, "%s was already applied; nothing changed\n", r.ApplicationID)
	} else {
		fmt.Fprintf(&b, "%s: catalog revision %d -> %d\n", r.ApplicationID, r.BaseRevision, r.AppliedRevision)
	}
	for _, c := range r.Changes {
		name := fmt.Sprintf("%s %s", c.Object, c.Key)
		if c.ProductKey != nil {
			name = fmt.Sprintf("price %s of product %s", c.Key, *c.ProductKey)
		}
		fmt.Fprintf(&b, "  changed %s (%s), now revision %d\n", name, strings.Join(c.Fields, ", "), c.Revision)
	}
	for _, line := range service.ReadableCatalogConflicts(r.Conflicts) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintf(&b, "%d object(s) skipped: an edit set the fields above. Change the file to agree, remove those fields, or apply with force.\n", len(r.Conflicts))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
