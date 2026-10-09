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

// CreateCreditGrants grants 1 to billing.MaxBatchItems credits, across any
// customers, all or none; the answer is in request order. Each item's
// SourceID makes it idempotent per customer: an identical retry answers the
// existing grant with Replayed set, one with a different amount, currency or
// expiry refuses the batch with billing.ErrIdempotencyKeyReused.
func (c *Client) CreateCreditGrants(ctx context.Context, items []billing.CreateCreditGrantParams, requestOptions ...RequestOption) ([]billing.CreditGrant, error) {
	if err := batchSize(len(items), billing.MaxBatchItems); err != nil {
		return nil, err
	}
	body := billing.CreateCreditGrantBatchParams{Items: make([]billing.CreateCreditGrantParams, len(items))}
	for i, item := range items {
		if item.CustomerID.IsZero() {
			return nil, invalidErr("customer_id is required")
		}
		item.Currency = normalizeCurrency(item.Currency)
		body.Items[i] = item
	}
	var out billing.CreateCreditGrantBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/credit-grants", body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out.Items, nil
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
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
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

// ListBalanceTransactions lists a customer's balance transactions in one currency,
// newest first, or the transactions params.IDs names.
func (c *Client) ListBalanceTransactions(ctx context.Context, customer billing.CustomerID, params billing.BalanceTransactionListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.BalanceTransaction], error) {
	path, err := customerIDPath(customer)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if params.IDs == nil || params.Currency != "" {
		q.Set("currency", normalizeCurrency(params.Currency))
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.BalanceTransaction]
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/balance/transactions", pageValues(q, params.PageRequest)), nil, &out, requestOptions...); err != nil {
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

// DeleteSpendDelegation revokes the delegation at scope and scopeKey; a
// missing one is billing.ErrNotFound.
func (c *Client) DeleteSpendDelegation(ctx context.Context, customer billing.CustomerID, scope billing.SpendDelegationScope, scopeKey string, requestOptions ...RequestOption) error {
	path, err := spendDelegationPath(customer, scope, scopeKey)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, requestOptions...)
}
