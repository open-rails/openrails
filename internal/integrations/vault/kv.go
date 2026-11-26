// Package vault provides the live HashiCorp Vault adapters, so the rest of the
// codebase never imports hashicorp/vault/api: KV v2 documents for merchant
// configuration (internal/merchantdocs) and Transit sign-as-a-service for the
// non-extractable Solana key (solana.TransitClient). The client is the host's,
// which keeps it authenticated.
package vault

import (
	"errors"
	"fmt"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
)

// KVv2Adapter reads and writes JSON documents on a KV v2 mount, addressed by
// paths under the mount.
type KVv2Adapter struct {
	client *vaultapi.Client
	mount  string
	// refused, when set, refuses every operation without a network call.
	refused error
	login   *FirstLogin
}

// NewKVv2Adapter builds a KV-v2 adapter for the given mount (e.g. "secret").
func NewKVv2Adapter(client *vaultapi.Client, mount string) *KVv2Adapter {
	return &KVv2Adapter{client: client, mount: strings.Trim(strings.TrimSpace(mount), "/")}
}

// Refuse makes every operation fail with err (wrapped in ErrUnavailable):
// Vault is named but cannot be used.
func (a *KVv2Adapter) Refuse(err error) *KVv2Adapter {
	if err != nil {
		a.refused = fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return a
}

// AwaitLogin holds each operation for the host's first login.
func (a *KVv2Adapter) AwaitLogin(l *FirstLogin) *KVv2Adapter {
	a.login = l
	return a
}

// BackendIdentity identifies the configured Vault address and namespace without
// credentials. Cleanup refuses a later configuration pointed at another backend.
func (a *KVv2Adapter) BackendIdentity() string {
	if a.client == nil {
		return ""
	}
	return strings.TrimRight(a.client.Address(), "/") + "|" + a.client.Namespace()
}

var (
	ErrPermissionDenied = errors.New("vault: credential access denied or token revoked")
	ErrSealed           = errors.New("vault: sealed")
	ErrUnavailable      = errors.New("vault: unavailable")
)

func credentialBackendError(err error) error {
	var response *vaultapi.ResponseError
	if errors.As(err, &response) {
		if response.StatusCode == 403 {
			return ErrPermissionDenied
		}
		if response.StatusCode == 503 {
			for _, message := range response.Errors {
				if strings.Contains(strings.ToLower(message), "sealed") {
					return ErrSealed
				}
			}
		}
	}
	return ErrUnavailable
}
