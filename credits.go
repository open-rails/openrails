package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// customerIDPath is the merchant route of one customer.
func customerIDPath(id billing.CustomerID) (string, error) {
	if id.IsZero() {
		return "", invalidErr("customer_id is required")
	}
	return "/v1/merchant/customers/" + id.String(), nil
}

// pageValues adds a list page's limit and cursor to q.
func pageValues(q url.Values, page billing.PageRequest) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if page.Limit != 0 {
		q.Set("limit", strconv.Itoa(page.Limit))
	}
	if page.Cursor != "" {
		q.Set("cursor", page.Cursor)
	}
	return q
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// CreateCreditGrant grants a customer prepaid credit. SourceID makes the grant
// idempotent: an identical retry returns the existing grant with Replayed set,
// one with a different amount, currency or expiry is
// billing.ErrIdempotencyKeyReused.
func (c *Client) CreateCreditGrant(ctx context.Context, customer billing.CustomerID, params billing.CreateCreditGrantParams, requestOptions ...RequestOption) (*billing.CreditGrant, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	params.Currency = normalizeCurrency(params.Currency)
	var out billing.CreditGrant
	if err := c.do(ctx, http.MethodPost, path+"/credit-grants", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCreditGrants lists a customer's credit grants, newest first.
func (c *Client) ListCreditGrants(ctx context.Context, customer billing.CustomerID, params billing.CreditGrantListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CreditGrant], error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if params.Currency != "" {
		q.Set("currency", normalizeCurrency(params.Currency))
	}
	if params.SourceID != "" {
		q.Set("source_id", params.SourceID)
	}
	var out billing.ListPage[billing.CreditGrant]
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/credit-grants", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func creditGrantPath(customer billing.CustomerID, id billing.CreditGrantID) (string, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return "", err
	}
	if id.IsZero() {
		return "", invalidErr("credit grant id is required")
	}
	return path + "/credit-grants/" + id.String(), nil
}

// GetCreditGrant reads one of a customer's credit grants.
func (c *Client) GetCreditGrant(ctx context.Context, customer billing.CustomerID, id billing.CreditGrantID, requestOptions ...RequestOption) (*billing.CreditGrant, error) {
	path, err := creditGrantPath(customer, id)
	if err != nil {
		return nil, err
	}
	var out billing.CreditGrant
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeCreditGrant revokes a grant's unspent remainder. Revoking a revoked
// grant returns it with Replayed set.
func (c *Client) RevokeCreditGrant(ctx context.Context, customer billing.CustomerID, id billing.CreditGrantID, params billing.RevokeCreditGrantParams, requestOptions ...RequestOption) (*billing.CreditGrant, error) {
	path, err := creditGrantPath(customer, id)
	if err != nil {
		return nil, err
	}
	var out billing.CreditGrant
	if err := c.do(ctx, http.MethodPost, path+"/revoke", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCreditTransactions lists a customer's credit ledger in one currency,
// newest first.
func (c *Client) ListCreditTransactions(ctx context.Context, customer billing.CustomerID, params billing.CreditTransactionListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CreditTransaction], error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	q := url.Values{"currency": {normalizeCurrency(params.Currency)}}
	var out billing.ListPage[billing.CreditTransaction]
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/transactions", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBalance reads a customer's balance, holds and owed amount in one
// currency.
func (c *Client) GetBalance(ctx context.Context, customer billing.CustomerID, currency string, requestOptions ...RequestOption) (*billing.Balance, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	var out billing.Balance
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/balance", url.Values{"currency": {normalizeCurrency(currency)}}), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCreditLimit returns how much a customer may owe in arrears in one
// currency.
func (c *Client) GetCreditLimit(ctx context.Context, customer billing.CustomerID, currency string, requestOptions ...RequestOption) (*billing.CreditLimit, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	var out billing.CreditLimit
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/credit-limit", url.Values{"currency": {normalizeCurrency(currency)}}), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCreditLimit sets how much a customer may owe in arrears.
func (c *Client) SetCreditLimit(ctx context.Context, customer billing.CustomerID, params billing.SetCreditLimitParams, requestOptions ...RequestOption) (*billing.CreditLimit, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	params.Currency = normalizeCurrency(params.Currency)
	var out billing.CreditLimit
	if err := c.do(ctx, http.MethodPut, path+"/credit-limit", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTrustLevel returns the trust level stored for a customer in one
// currency.
func (c *Client) GetTrustLevel(ctx context.Context, customer billing.CustomerID, currency string, requestOptions ...RequestOption) (*billing.TrustLevel, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	var out billing.TrustLevel
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/trust-level", url.Values{"currency": {normalizeCurrency(currency)}}), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetTrustLevel stores the trust level a customer's admissions use when a
// request names none; an empty level clears it.
func (c *Client) SetTrustLevel(ctx context.Context, customer billing.CustomerID, params billing.SetTrustLevelParams, requestOptions ...RequestOption) (*billing.TrustLevel, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	params.Currency = normalizeCurrency(params.Currency)
	var out billing.TrustLevel
	if err := c.do(ctx, http.MethodPut, path+"/trust-level", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSpendDelegations lists the delegations that let invokers spend a
// customer's balance.
func (c *Client) ListSpendDelegations(ctx context.Context, customer billing.CustomerID, requestOptions ...RequestOption) (*billing.ListPage[billing.SpendDelegation], error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.SpendDelegation]
	if err := c.do(ctx, http.MethodGet, path+"/spend-delegations", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSpendDelegations replaces a customer's spend delegations.
func (c *Client) SetSpendDelegations(ctx context.Context, customer billing.CustomerID, delegations []billing.SpendDelegation, requestOptions ...RequestOption) (*billing.ListPage[billing.SpendDelegation], error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	if delegations == nil {
		delegations = []billing.SpendDelegation{}
	}
	var out billing.ListPage[billing.SpendDelegation]
	if err := c.do(ctx, http.MethodPut, path+"/spend-delegations", billing.SetSpendDelegationsParams{Delegations: delegations}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func spendDelegationPath(customer billing.CustomerID, scope billing.SpendDelegationScope, scopeKey string) (string, error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return "", err
	}
	scopeSegment, err := pathID("scope", string(scope))
	if err != nil {
		return "", err
	}
	keySegment, err := pathID("scope_key", scopeKey)
	if err != nil {
		return "", err
	}
	return path + "/spend-delegations/" + scopeSegment + "/" + keySegment, nil
}

// SetSpendDelegation sets the delegation at its scope and key, leaving the
// customer's other delegations untouched.
func (c *Client) SetSpendDelegation(ctx context.Context, customer billing.CustomerID, delegation billing.SpendDelegation, requestOptions ...RequestOption) (*billing.SpendDelegation, error) {
	path, err := spendDelegationPath(customer, delegation.Scope, delegation.ScopeKey)
	if err != nil {
		return nil, err
	}
	var out billing.SpendDelegation
	body := billing.SetSpendDelegationParams{Windows: delegation.Windows, Provenance: delegation.Provenance}
	if err := c.do(ctx, http.MethodPut, path, body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSpendDelegation revokes the delegation at scope and scopeKey; a
// missing one is billing.ErrNotFound.
func (c *Client) DeleteSpendDelegation(ctx context.Context, customer billing.CustomerID, scope billing.SpendDelegationScope, scopeKey string, requestOptions ...RequestOption) error {
	path, err := spendDelegationPath(customer, scope, scopeKey)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, requestOptions...)
}
