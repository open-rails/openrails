package openrails

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CredentialTarget is the merchant one operation addresses, by exactly one of
// its slug and ID. It is not verified authority: the server resolves the
// merchant and authorizes the credential independently.
type CredentialTarget struct {
	MerchantSlug string
	MerchantID   billing.MerchantID
}

// RequestOption configures one operation without changing the shared Client.
type RequestOption func(*requestOptions)

type requestOptions struct {
	target         CredentialTarget
	selected       bool
	idempotencyKey string
	err            error
}

// WithIdempotencyKey sends key as a programmatic write's Idempotency-Key: a
// retry with it answers the first response. Without it each call sends its
// own key, and the operation's own ids (request, source, operation) keep a
// retry safe.
func WithIdempotencyKey(key string) RequestOption {
	key = strings.TrimSpace(key)
	return func(o *requestOptions) { o.idempotencyKey = key }
}

// idempotencyKey is the key options name, "" for none.
func idempotencyKey(options []RequestOption) string {
	request := requestOptions{}
	for _, option := range options {
		if option != nil {
			option(&request)
		}
	}
	return request.idempotencyKey
}

func (o *requestOptions) selectMerchant(target CredentialTarget, err error) {
	if o.err != nil {
		return
	}
	if o.selected {
		o.err = invalidErr("specify exactly one merchant selector per operation")
		return
	}
	o.selected, o.target, o.err = true, target, err
}

// WithMerchant selects a merchant by slug for this operation only. It overrides
// the Client's default, but never a credential or deployment restriction.
func WithMerchant(slug string) RequestOption {
	slug = billing.NormalizeMerchantSlug(slug)
	var err error
	if e := billing.ValidateMerchantSlug(slug); e != nil {
		err = invalidErr(e.Error())
	}
	return func(o *requestOptions) {
		o.selectMerchant(CredentialTarget{MerchantSlug: slug}, err)
	}
}

// ForMerchantID selects a stable merchant UUID for a single operation. Use it
// for stored merchant identities; use WithMerchant for human-facing slugs.
func ForMerchantID(id billing.MerchantID) RequestOption {
	var err error
	if id.IsZero() {
		err = invalidErr("merchant ID must not be zero")
	}
	return func(o *requestOptions) {
		o.selectMerchant(CredentialTarget{MerchantID: id}, err)
	}
}

// WithDefaultMerchant supplies an immutable fallback slug for operations that
// omit a selector. An explicit request selector may override it. This is not
// an authority grant or a restriction on the merchants served by the runtime.
func WithDefaultMerchant(slug string) ClientOption {
	slug = billing.NormalizeMerchantSlug(slug)
	return func(c *Client) {
		if err := billing.ValidateMerchantSlug(slug); err != nil {
			c.setupErr = invalidErr(err.Error())
			return
		}
		c.merchantSlug = slug
		c.merchantID = billing.MerchantID{}
	}
}

// WithCredentialProvider obtains the Bearer token for the requested merchant
// on each call. It replaces any previously configured token source; failure
// never falls back to a broader credential. The callback must be concurrency
// safe. It must not convert sender-constrained credentials into Bearer tokens.
func WithCredentialProvider(fn func(context.Context, CredentialTarget) (string, error)) ClientOption {
	return func(c *Client) {
		if fn == nil {
			c.setupErr = fmt.Errorf("openrails: credential provider is required")
			return
		}
		c.credentialFn, c.tokenFn = fn, nil
	}
}

func (c *Client) requestTarget(options []RequestOption) (CredentialTarget, error) {
	request := requestOptions{}
	for _, option := range options {
		if option != nil {
			option(&request)
		}
	}
	if request.err != nil {
		return CredentialTarget{}, request.err
	}
	if !request.selected {
		request.target = CredentialTarget{MerchantSlug: c.merchantSlug, MerchantID: c.merchantID}
	}
	if request.target.MerchantSlug == "" && request.target.MerchantID.IsZero() {
		return CredentialTarget{}, invalidErr("merchant is required: use WithMerchant, ForMerchantID, or a Client default")
	}
	return request.target, nil
}
