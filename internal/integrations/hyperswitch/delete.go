package hyperswitch

import (
	"context"
	"net/http"
	"net/url"
)

// DeleteMethod completes one exact native-vault deletion. The caller must first
// durably accept and fence the owned instrument; a redacted vendor record alone
// does not prove the card has been physically erased. The qualified native
// DELETE is safely repeatable after both interrupted work and a lost response.
// External vaults and network-token copies have no qualified erasure contract.
func (c *Client) DeleteMethod(ctx context.Context, id string) error {
	if !safeIdentifier(id) {
		return ErrBinding
	}
	if c.readOnly {
		return ErrReadOnly
	}
	var contract proxyContract
	if err := c.call(ctx, http.MethodGet, "/v2/proxy", nil, &contract); err != nil {
		return ErrUnavailable
	}
	// Deletion must remain available when the operator disables money routes.
	// It depends on the authenticated native-vault capability, not their list.
	if contract.Contract != "openrails-nmi-form-v2" || contract.NativeVaultDeleteContract != "openrails-native-vault-delete-v1" {
		return ErrUnavailable
	}
	var deleted struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodDelete, "/v2/payment-methods/"+url.PathEscape(id), nil, &deleted); err != nil {
		// In particular, a missing logical record or a refusal says nothing
		// about physical erasure. Only the exact successful receipt completes.
		return ErrUnknown
	}
	if deleted.ID != id {
		return ErrUnknown
	}
	return nil
}
