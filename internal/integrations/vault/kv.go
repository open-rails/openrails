// Package vault provides the live HashiCorp Vault adapters, so the rest of the
// codebase never imports hashicorp/vault/api: KV v2 documents for merchant
// configuration (internal/merchantdocs) and Transit sign-as-a-service for the
// non-extractable Solana key (solana.TransitClient). It authenticates ONCE as
// the OpenRails process (AppRole / K8s; see auth.go) and is the trusted broker.
package vault

import (
	"errors"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
)

// KVv2Adapter reads and writes JSON documents on a KV v2 mount, addressed by
// paths under the mount.
type KVv2Adapter struct {
	client *vaultapi.Client
	mount  string

	// onPermissionDenied is called with every error a KV operation observes,
	// letting a wired Supervisor decide (via NotifyPermissionDenied) whether
	// it signals real token death (#751 task 5). Nil (the zero value / a bare
	// NewKVv2Adapter) is a valid no-op — tests and one-off root/dedicated-
	// container clients never need it.
	onPermissionDenied func(error)
	sup                *Supervisor
}

// NewKVv2Adapter builds a KV-v2 adapter for the given mount (e.g. "secret").
func NewKVv2Adapter(client *vaultapi.Client, mount string) *KVv2Adapter {
	return &KVv2Adapter{client: client, mount: strings.Trim(strings.TrimSpace(mount), "/")}
}

// WithReauthTrigger wires the adapter so every failed KV operation reports
// its error to sup.NotifyPermissionDenied (#751 task 5): a permission-denied
// response whose self-lookup confirms the token is dead triggers an
// immediate re-auth instead of waiting out the current lease or the
// merchant-secret cache TTL. Returns the adapter for chaining at
// construction (e.g. merchantsecrets/store.go:
// NewKVv2Adapter(client, mount).WithReauthTrigger(sup)). A nil sup is a no-op.
func (a *KVv2Adapter) WithReauthTrigger(sup *Supervisor) *KVv2Adapter {
	if sup != nil {
		a.onPermissionDenied = sup.NotifyPermissionDenied
		a.sup = sup
	}
	return a
}

func (a *KVv2Adapter) notifyErr(err error) {
	if err != nil && a.onPermissionDenied != nil {
		a.onPermissionDenied(err)
	}
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
