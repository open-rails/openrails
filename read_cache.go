package openrails

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// transport is how a handle reaches OpenRails, which sets what a read costs.
type transport string

const (
	inProcess transport = "embedded"
	overHTTP  transport = "http"
)

// readPolicies is the default policy of every cached read, by transport: an
// in-process read costs a database query, a remote one an HTTP round trip.
// A read a transport does not list reads live there. Customer and money
// reads are never cached.
var readPolicies = map[transport]ReadCache{
	inProcess: {
		ReadCatalog: {RefreshAfter: time.Minute, MaxStale: 10 * time.Minute},
		ReadMeters:  {RefreshAfter: time.Minute, MaxStale: 10 * time.Minute},
		// The engine already serves public and merchant configuration from memory.
	},
	overHTTP: {
		ReadCatalog:               {RefreshAfter: 10 * time.Minute, MaxStale: time.Hour},
		ReadMeters:                {RefreshAfter: 10 * time.Minute, MaxStale: time.Hour},
		ReadPublicConfig:          {RefreshAfter: 10 * time.Minute, MaxStale: time.Hour},
		ReadMerchantConfiguration: {RefreshAfter: 30 * time.Second, MaxStale: 10 * time.Minute},
	},
}

// readChanges are the writes that change cached reads: a handle's own write
// at one of these paths drops what its transport holds for the merchant.
var readChanges = []string{"/v1/catalog/", "/v1/merchant/configuration", "/v1/merchant/billing-archive"}

// reads serves one transport's cached reads for each merchant it reaches.
type reads struct {
	// handle is the handle loads use for a merchant.
	handle func(billing.MerchantID) *Client
	// gens advance on each change to a merchant, so its old answers are
	// never read again.
	gens    sync.Map // billing.MerchantID -> *atomic.Uint64
	answers map[Read]*cache.Cache[readKey, answer]
	// products are ListProducts' items by key or id, cached one by one.
	products *cache.Cache[itemKey, answer]
}

type readKey struct {
	merchant billing.MerchantID
	gen      uint64
	path     string
}

// itemKey is one item of a list read: by is its batch parameter (keys or
// ids) and query the read's other parameters.
type itemKey struct {
	merchant billing.MerchantID
	gen      uint64
	query    string
	by       string
	value    string
}

// answer is a cached response: its body, or the refusal the server answered.
// An item answer with neither is an item that does not exist.
type answer struct {
	body    []byte
	refusal *billing.StatusError
}

// loadError carries a load's own error through the cache's wrapping.
type loadError struct{ err error }

func (e loadError) Error() string { return e.err.Error() }
func (e loadError) Unwrap() error { return e.err }

// newReads builds t's caches under overrides; timeout bounds each load (zero:
// the cache default). An override it cannot serve keeps its read's default
// and is named in refused.
func newReads(t transport, overrides ReadCache, timeout time.Duration, handle func(billing.MerchantID) *Client) (_ *reads, refused []config.Degradation, err error) {
	policies, refused := readPoliciesFor(t, overrides)
	r := &reads{handle: handle, answers: map[Read]*cache.Cache[readKey, answer]{}}
	for read, policy := range policies {
		options := cache.Options{Name: "openrails." + string(read) + "." + string(t), RefreshAfter: policy.RefreshAfter, MaxStale: policy.MaxStale, LoadTimeout: timeout}
		if r.answers[read], err = cache.New(options, r.loadAnswers); err != nil {
			return nil, nil, err
		}
		if read == ReadCatalog {
			options.Name = "openrails.products." + string(t)
			if r.products, err = cache.New(options, r.loadProducts); err != nil {
				return nil, nil, err
			}
		}
	}
	return r, refused, nil
}

// readPoliciesFor is t's default policies with overrides applied; a zero
// override turns its read off. An override naming no read, or whose bounds
// are out of order, is ignored and named in refused.
func readPoliciesFor(t transport, overrides ReadCache) (ReadCache, []config.Degradation) {
	out := ReadCache{}
	for read, policy := range readPolicies[t] {
		out[read] = policy
	}
	var refused []config.Degradation
	for _, read := range slices.Sorted(maps.Keys(overrides)) {
		policy := overrides[read]
		field := "read_cache." + string(read)
		if _, ok := readPolicies[overHTTP][read]; !ok {
			refused = append(refused, config.Degradation{Field: field, Err: fmt.Errorf("names an unknown read; ignored")})
			continue
		}
		switch {
		case policy == ReadPolicy{}:
			delete(out, read)
		case policy.RefreshAfter <= 0 || policy.MaxStale <= policy.RefreshAfter:
			refused = append(refused, config.Degradation{Field: field, Err: fmt.Errorf("needs 0 < RefreshAfter < MaxStale; the default applies")})
		default:
			out[read] = policy
		}
	}
	return out, refused
}

// close releases the caches' background work; they keep answering.
func (r *reads) close() {
	if r == nil {
		return
	}
	for _, c := range r.answers {
		c.Close()
	}
	if r.products != nil {
		r.products.Close()
	}
}

func (r *reads) counter(id billing.MerchantID) *atomic.Uint64 {
	g, ok := r.gens.Load(id)
	if !ok {
		g, _ = r.gens.LoadOrStore(id, new(atomic.Uint64))
	}
	return g.(*atomic.Uint64)
}

func (r *reads) gen(id billing.MerchantID) uint64 { return r.counter(id).Load() }

// forget drops every answer held for merchant id.
func (r *reads) forget(id billing.MerchantID) {
	if r != nil {
		r.counter(id).Add(1)
	}
}

func (r *reads) cacheOf(read Read) *cache.Cache[readKey, answer] {
	if r == nil {
		return nil
	}
	return r.answers[read]
}

// read is a GET of path that read's policy may answer from memory. Cached,
// requestOptions are not sent: no read takes one.
func (c *Client) read(ctx context.Context, read Read, path string, out any, requestOptions ...RequestOption) error {
	answers := c.reads.cacheOf(read)
	if answers == nil || ctx.Err() != nil {
		return c.do(ctx, http.MethodGet, path, nil, out, requestOptions...)
	}
	key := readKey{merchant: c.merchantID, gen: c.reads.gen(c.merchantID), path: path}
	got, err := answers.Get(ctx, []readKey{key})
	a, ok := got[key]
	switch {
	case !ok:
		return readFailure(ctx, err)
	case a.refusal != nil:
		refusal := *a.refusal
		return &refusal
	}
	return decodeJSON(a.body, out)
}

// readFailure is the error a load returned, as the uncached call returns it.
func readFailure(ctx context.Context, err error) error {
	var failed loadError
	switch {
	case errors.As(err, &failed):
		return failed.err
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, ctx.Err())
	case err == nil:
		return fmt.Errorf("%w: the read returned nothing", billing.ErrUnreachable)
	}
	return err
}

// refusal is err when it is the server's settled answer, a 4xx that the same
// request would get again; a transient failure is nil.
func refusal(err error) *billing.StatusError {
	var status *billing.StatusError
	if !errors.As(err, &status) || status.Status < 400 || status.Status >= 500 {
		return nil
	}
	switch status.Status {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return nil
	}
	return status
}

func (r *reads) loadAnswers(ctx context.Context, keys []readKey) (map[readKey]answer, error) {
	out := make(map[readKey]answer, len(keys))
	var errs []error
	for _, key := range keys {
		resp, err := r.handle(key.merchant).doResponse(ctx, http.MethodGet, key.path, nil, nil)
		if err == nil {
			// A body the read cannot decode is the transport failing, not an answer.
			err = decodeJSON(resp.body, new(json.RawMessage))
		}
		switch status := refusal(err); {
		case err == nil:
			out[key] = answer{body: resp.body}
		case status != nil:
			out[key] = answer{refusal: status}
		default:
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return out, loadError{errs[0]}
	}
	return out, nil
}

// productBatch is the batch parameter ListProducts answers item by item:
// ids alone, or keys that fit on one page. ok is false for any other list.
func productBatch(params billing.ProductListParams) (by string, values []string, ok bool) {
	switch {
	case params.IDs != nil:
		if params.PageRequest != (billing.PageRequest{}) || len(params.Keys) > 0 || params.Archived != nil || params.TierGroup != "" || len(params.Entitlements) > 0 || params.ForSale != nil {
			return "", nil, false
		}
		for _, id := range params.IDs {
			values = append(values, id.String())
		}
		by = "ids"
	case len(params.Keys) > 0 && params.Cursor == "" && params.Limit >= 0 && params.Limit <= billing.MaxPageLimit:
		values, by = slices.Clone(params.Keys), "keys"
	default:
		return "", nil, false
	}
	slices.Sort(values)
	values = slices.Compact(values)
	if by == "keys" && len(values) > cmp.Or(params.Limit, billing.DefaultPageLimit) {
		return "", nil, false
	}
	return by, values, true
}

// readProducts answers ListProducts from its items, loading the ones not
// cached in one request, in the server's order: newest first, then by id.
func (c *Client) readProducts(ctx context.Context, query, by string, values []string) (*billing.ListPage[billing.Product], error) {
	gen := c.reads.gen(c.merchantID)
	keys := make([]itemKey, len(values))
	for i, value := range values {
		keys[i] = itemKey{merchant: c.merchantID, gen: gen, query: query, by: by, value: value}
	}
	got, err := c.reads.products.Get(ctx, keys)
	out := &billing.ListPage[billing.Product]{Items: make([]billing.Product, 0, len(keys))}
	for _, key := range keys {
		a, ok := got[key]
		switch {
		case !ok:
			return nil, readFailure(ctx, err)
		case a.refusal != nil:
			refusal := *a.refusal
			return nil, &refusal
		case a.body == nil:
			continue
		}
		var product billing.Product
		if err := decodeJSON(a.body, &product); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, product)
	}
	slices.SortFunc(out.Items, func(a, b billing.Product) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		x, y := uuid.UUID(a.ID), uuid.UUID(b.ID)
		return bytes.Compare(y[:], x[:])
	})
	return out, nil
}

// loadProducts reads the products keys name, one request per list.
func (r *reads) loadProducts(ctx context.Context, keys []itemKey) (map[itemKey]answer, error) {
	type list struct {
		merchant  billing.MerchantID
		gen       uint64
		query, by string
	}
	lists := map[list][]itemKey{}
	for _, key := range keys {
		l := list{key.merchant, key.gen, key.query, key.by}
		lists[l] = append(lists[l], key)
	}
	out := make(map[itemKey]answer, len(keys))
	var errs []error
	for l, items := range lists {
		q, err := url.ParseQuery(l.query)
		if err != nil {
			return out, loadError{err}
		}
		values := make([]string, len(items))
		for i, item := range items {
			values[i] = item.value
		}
		if l.by == "ids" {
			q.Set("ids", strings.Join(values, ","))
		} else {
			q["keys"] = values
			q.Set("limit", strconv.Itoa(len(values)))
		}
		var page struct {
			Data []json.RawMessage `json:"data"`
		}
		resp, err := r.handle(l.merchant).doResponse(ctx, http.MethodGet, "/v1/catalog/products?"+q.Encode(), nil, nil)
		if err == nil {
			err = decodeJSON(resp.body, &page)
		}
		if status := refusal(err); status != nil {
			for _, item := range items {
				out[item] = answer{refusal: status}
			}
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		found := make(map[string]json.RawMessage, len(page.Data))
		for _, raw := range page.Data {
			var head struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return out, loadError{fmt.Errorf("%w: decode response: %w", billing.ErrUnreachable, err)}
			}
			if l.by == "ids" {
				found[head.ID] = raw
			} else {
				found[head.Key] = raw
			}
		}
		for _, item := range items {
			out[item] = answer{body: found[item.value]}
		}
	}
	if len(errs) > 0 {
		return out, loadError{errs[0]}
	}
	return out, nil
}

// changesReads reports whether a request of method at path can change what
// the handle's transport caches.
func changesReads(method, path string) bool {
	return method != http.MethodGet && slices.ContainsFunc(readChanges, func(prefix string) bool { return strings.HasPrefix(path, prefix) })
}

// decodeJSON decodes the one JSON value of a response body into out.
func decodeJSON(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: decode response: %w", billing.ErrUnreachable, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: response must contain one JSON value", billing.ErrUnreachable)
	}
	return nil
}
