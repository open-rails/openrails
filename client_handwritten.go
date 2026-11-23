package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	auth "github.com/open-rails/authkit/auth"
	"github.com/open-rails/helpers/eventlog"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/wireform"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

// The Client methods client_gen.go does not generate: the handle's own, the
// customer tokens and the event reader, which call no route of their own,
// and the routes declared HandWritten, which stream or are answered from
// memory.

// MerchantID is the handle's merchant.
func (c *Client) MerchantID() billing.MerchantID { return c.merchantID }

// ActingFor is the in-process handle whose calls act for invoker, the person
// signed in at the host, by issuer and subject as the host's identity
// provider verified them. Each call is then theirs, as their own HTTP call
// would be: their destructive-action limits and holds apply, and a route
// that needs a person, such as ClearDestructiveHold, admits them. A handle
// naming no one is the host's own work: audited, never counted or held. An
// issuer beginning "openrails:" or an empty name is refused at the call
// (host_principal_invalid). A remote handle refuses: its credential names
// its caller.
func (c *Client) ActingFor(invoker auth.Invoker) (*Client, error) {
	switch {
	case c == nil || c.client == nil:
		return nil, fmt.Errorf("openrails: a Client comes from Engine.Merchant or Remote.Merchant")
	case !c.inprocess:
		return nil, fmt.Errorf("openrails: ActingFor is for an in-process handle; a remote handle's credential names its caller")
	}
	d := c.derive()
	d.invoker = &invoker
	return d, nil
}

// Uncached is the handle reading everything from OpenRails: no catalog or
// configuration read and no entitlement check is answered from memory. Its
// answers still refresh the entitlement cache (CheckEntitlementsLive), and its
// writes drop what the shared caches hold for the merchant.
func (c *Client) Uncached() *Client {
	if c == nil {
		return nil
	}
	d := c.derive()
	d.uncached = true
	return d
}

// CustomerAssertion signs the assertion that names params.CustomerID as the
// merchant's customer, for a frontend to redeem itself at the token endpoint
// (grant_type urn:ietf:params:oauth:grant-type:jwt-bearer, scope
// openrails:self), with its own DPoP proof when it binds the token to a key.
// It is valid for two minutes and redeems once. It makes no request. It fails
// on a handle whose credential has no Assertion and on an in-process handle,
// whose host mints with its own AuthKit.
func (c *Client) CustomerAssertion(ctx context.Context, params billing.CustomerTokenParams) (string, error) {
	return c.customerAssertion(ctx, params)
}

// CustomerToken mints an access token for params.CustomerID as the merchant's
// customer: it signs a CustomerAssertion and redeems it at the credential's
// Assertion.TokenURL. The backend hands the token to its frontend, which sends
// it to the customer routes as its Bearer. It fails on a handle whose
// credential has no Assertion and on an in-process handle, whose host mints
// with its own AuthKit. A refusal is a *billing.StatusError carrying the token
// endpoint's status and its OAuth error as the message, with no code.
func (c *Client) CustomerToken(ctx context.Context, params billing.CustomerTokenParams) (*billing.CustomerToken, error) {
	return c.customerToken(ctx, params)
}

// EventReader adapts the merchant client to the shared consumer interface. The
// same reader works for Engine.Merchant and remote HTTP clients; credentials,
// authorization and merchant binding remain in the client transport.
func (c *Client) EventReader() eventlog.Reader {
	return eventlog.ReaderFunc(func(ctx context.Context, q eventlog.Query) (eventlog.Page, error) {
		normalized, err := eventlog.Normalize(q)
		if err != nil {
			return eventlog.Page{}, err
		}
		p, err := c.ListEvents(ctx, billing.EventQuery{After: normalized.After, Types: normalized.Types, Limit: normalized.Limit})
		if err != nil {
			var status *billing.StatusError
			if errors.As(err, &status) {
				switch status.Code {
				case billing.CodeInvalidCursor:
					return eventlog.Page{}, errors.Join(eventlog.ErrInvalidCursor, err)
				case billing.CodeEventCursorExpired:
					return eventlog.Page{}, errors.Join(eventlog.ErrExpiredCursor, err)
				}
			}
			return eventlog.Page{}, err
		}
		out := eventlog.Page{Events: []eventlog.Event{}, NextCursor: p.NextCursor, HasMore: p.HasMore, RetentionSeconds: p.RetentionSeconds}
		for _, e := range p.Events {
			out.Events = append(out.Events, eventlog.Event{ID: e.ID, Source: e.Source, Scope: e.MerchantID.String(), Type: e.Type, Version: e.Version, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Subject: eventlog.Subject{Type: e.Subject.Type, ID: e.Subject.ID}, Data: e.Data})
		}
		return out, nil
	})
}

// CheckEntitlements is the programmatic content gate: which of
// params.Entitlements params.CustomerID holds, and the keys they hold under
// each of params.Prefixes, at params.At (zero: now); every requested key and
// prefix is in the answer. One call checks at most
// billing.MaxEntitlementChecks keys and billing.MaxEntitlementPrefixes
// prefixes; more is refused with invalid_param, not split. It needs an
// application holding billing.MerchantEntitlementsRead or RootEntitlementsRead
// (RouteGroups.Programmatic). A remote client answers a current exact-key
// check from its entitlement cache when every key is allowed there
// (EntitlementCheckCacheOptions), unless the handle is Uncached; a denial
// always asks OpenRails.
func (c *Client) CheckEntitlements(ctx context.Context, params billing.CheckEntitlementsParams) (*billing.EntitlementCheck, error) {
	return c.checkEntitlements(ctx, params, c == nil || !c.uncached)
}

// CheckEntitlementsLive checks access against OpenRails, never answering from
// the SDK cache. Its answer replaces allowed keys' entries and drops denied
// ones; a fill begun before it cannot restore them. The drop reaches this
// client's own memory at once, or the shared Redis within a bounded wait; if
// Redis fails, other replicas may serve the old answer for up to MaxAge. Use
// it when a revocation or credential change must be visible immediately.
func (c *Client) CheckEntitlementsLive(ctx context.Context, params billing.CheckEntitlementsParams) (*billing.EntitlementCheck, error) {
	return c.checkEntitlements(ctx, params, false)
}

func (c *Client) checkEntitlements(ctx context.Context, params billing.CheckEntitlementsParams, useCache bool) (*billing.EntitlementCheck, error) {
	if params.Entitlements == nil {
		params.Entitlements = []string{}
	}
	// The cache answers only a check the server would answer; it refuses the
	// rest itself.
	if c != nil && c.entitlementChecks != nil && entitlements.ValidateCheck(params) == nil {
		if keys := cacheableEntitlementKeys(params); keys != nil {
			return c.checkEntitlementsCached(ctx, params, keys, !useCache)
		}
	}
	var out billing.EntitlementCheck
	if err := c.send(ctx, routeCall{method: http.MethodPost, path: entitlementCheckPath, body: params}, &out); err != nil {
		return nil, err
	}
	return normalizeEntitlementCheck(&out), nil
}

func normalizeEntitlementCheck(out *billing.EntitlementCheck) *billing.EntitlementCheck {
	if out.Entitlements == nil {
		out.Entitlements = map[string]bool{}
	}
	if out.Quantities == nil {
		out.Quantities = map[string]*int{}
	}
	if out.Held == nil {
		out.Held = map[string]billing.HeldEntitlements{}
	}
	return out
}

// ListProducts returns one page of products, newest first, each with its
// current prices. params.Keys reads products by key. A read of named products
// that fits on one page is answered from memory product by product.
func (c *Client) ListProducts(ctx context.Context, params billing.ProductListParams) (*billing.ListPage[billing.Product], error) {
	if by, values, ok := productBatch(params); ok && c.reads != nil && c.reads.products != nil && !c.uncached && ctx.Err() == nil {
		filter := params
		filter.PageRequest, filter.IDs, filter.Keys = billing.PageRequest{}, nil, nil
		q, err := wireform.Encode(filter)
		if err != nil {
			return nil, fmt.Errorf("openrails: encode query: %w", err)
		}
		return c.readProducts(ctx, q.Encode(), by, values)
	}
	var out billing.ListPage[billing.Product]
	if err := c.send(ctx, routeCall{method: http.MethodGet, path: "/v1/catalog/products", query: params, cache: config.ReadCatalog}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPrice reads a price; params.Verify also reads a provider price's terms
// at its provider and reports their drift, never from memory.
func (c *Client) GetPrice(ctx context.Context, id billing.PriceID, params billing.GetPriceParams) (*billing.Price, error) {
	call := routeCall{method: http.MethodGet, path: "/v1/catalog/prices/{id}", args: []string{id.String()}, query: params, cache: config.ReadCatalog}
	if params.Verify {
		call.cache = ""
	}
	var out billing.Price
	if err := c.send(ctx, call, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

const merchantBillingArchivePath = "/v1/merchant/billing-archive"

// ExportBillingArchive writes the bound merchant's versioned billing archive.
// Stop source writers for the final cutover export. Authentication identities,
// provider credentials and live jobs are not portable billing records.
//
// The writer may contain incomplete bytes when an error is returned; publish a
// file only after success. The Client verifies the archive's integrity footer,
// and uses the same credential, merchant binding and timeout as other methods.
func (c *Client) ExportBillingArchive(ctx context.Context, dst io.Writer) error {
	if dst == nil {
		return invalidErr("archive writer is required")
	}
	return c.withHTTPResponse(ctx, http.MethodGet, merchantBillingArchivePath, nil, http.Header{"Accept": {"application/x-ndjson"}}, func(resp *http.Response) error {
		if err := archiveResponseError(resp); err != nil {
			return err
		}
		if _, err := archivewire.CopyVerified(dst, resp.Body); err != nil {
			return fmt.Errorf("%w: incomplete or invalid billing archive: %w", billing.ErrUnreachable, err)
		}
		return nil
	})
}

// ImportBillingArchive atomically restores an archive into an empty destination
// with the same merchant UUID. Provision destination authority separately and
// keep destination workers stopped through restoration and reconfiguration.
// No provider call is made. A lost response can be retried with the same archive.
func (c *Client) ImportBillingArchive(ctx context.Context, src io.Reader) (*billing.BillingArchiveImport, error) {
	if src == nil {
		return nil, invalidErr("archive reader is required")
	}
	var result billing.BillingArchiveImport
	headers := http.Header{"Content-Type": {"application/x-ndjson"}}
	err := c.withHTTPResponse(ctx, http.MethodPost, merchantBillingArchivePath, src, headers, func(resp *http.Response) error {
		if err := archiveResponseError(resp); err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil {
			return fmt.Errorf("%w: read archive receipt: %w", billing.ErrUnreachable, err)
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("%w: archive receipt exceeds 1 MiB", billing.ErrUnreachable)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if err := decoder.Decode(&result); err != nil {
			return fmt.Errorf("%w: decode archive receipt: %w", billing.ErrUnreachable, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("%w: archive receipt must contain one JSON value", billing.ErrUnreachable)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func archiveResponseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read archive refusal: %w", billing.ErrUnreachable, err)
	}
	failure := statusErrorFromBody(resp.StatusCode, raw).(*billing.StatusError)
	if failure.RequestID == "" {
		failure.RequestID = resp.Header.Get("X-Request-ID")
	}
	failure.RetryAfter = resp.Header.Get("Retry-After")
	return failure
}
