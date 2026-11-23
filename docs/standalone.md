# Standalone OpenRails

Run OpenRails as a separate service when your application should call billing
over HTTP. You operate OpenRails and its PostgreSQL database. Your app can
keep its own sign-in and issue credentials that OpenRails trusts for one
merchant. Go developers use the same `*openrails.Client` as the [embedded
integration](embedded.md), and other backends can call the [HTTP API](api/routes.md).

## Run it

Use the `openrails` binary from the [v0.282.0 release](https://github.com/open-rails/openrails/releases/tag/v0.282.0)
or the image `docker.io/openrails/openrails:v0.282.0`. The service needs
PostgreSQL 18 or newer and shared Redis when running multiple instances.
See [requirements](operations.md#requirements) and [Kubernetes](kubernetes.md).

For a local check with no payment processor, start with this `config.yaml`.
Use a writable directory for the development keys and keep the directory ID
stable across restarts. Generate your own ID for a new deployment.

```yaml
host: 127.0.0.1
port: 3053
provider_write_mode: readonly
public_billing_base_url: http://localhost:3053

auth:
  default_tenant_id: 51336692-ef57-4cdb-a189-a8d388465c11
  token:
    issuer: http://localhost:3053
    allow_private_network_jwks: true
  keys:
    path: ./openrails-keys
    allow_ephemeral_dev_keys: true
  http:
    direct_peer_ip: true
  registration:
    allow_missing_senders: true
  resource:
    id: http://localhost:3053
    public_url: http://localhost:3053

route_groups:
  catalog: true
```

Save the manifest below as `merchant.yaml`. Then supply `DB_URL` from your
application environment or secrets manager and start the service. The server
migrates the database at boot, loads the merchant and starts workers.

```sh
DB_URL="$OPENRAILS_DATABASE_URL" openrails run-server --config config.yaml --merchant-manifest merchant.yaml
```

In a second shell, check readiness and the public API. Both should return 200.

```sh
curl --fail-with-body http://localhost:3053/health/ready
curl --fail-with-body http://localhost:3053/v1/config
```

The `allow_*` settings above are for local development. In production, use
HTTPS issuer and resource URLs, managed signing keys and database secrets.
Behind a reverse proxy, set `auth.http.direct_peer_ip: false` and configure
`trusted_proxies`. See
[configuration](configuration.md) and [sessions and proxies](auth.md#sessions-and-proxies).

## The merchant

The manifest names the merchant and the issuer whose tokens act for it. This
example assumes your app runs its issuer on port 8080 and serves its public
keys at the standard AuthKit JWKS path.

```yaml
version: 1
merchants:
  myapp:
    display_name: My App
    remote_application:
      issuer: http://localhost:8080
      jwks_uri: http://localhost:8080/.well-known/jwks.json
```

The server creates the merchant on first boot and reads the file on restart.
Add PSPs when you are ready to integrate payments. Keep PSP credentials in
secret overlays or use Vault for configuration edited at runtime. See
[merchant configuration](merchant-configuration.md), [manifest
overlays](configuration.md#merchant-configuration-in-a-file) and [Vault](vault.md).

Apply a catalog as a separate operator step after the merchant exists.

```sh
DB_URL="$OPENRAILS_DATABASE_URL" openrails catalog apply catalog.yaml \
  --config config.yaml --merchant-manifest merchant.yaml --merchant myapp
```

Enable other route groups as needed, for example `programmatic` for `/v1/app/*`
or `admin` for customer support. The standalone console at `/admin` can use a
trusted issuer or the server's own sign-in when enabled. See [billing
admin](billing-admin.md) and [commands](operations.md#commands).

## Trust your app's AuthKit

Use the manifest's `remote_application.issuer` as your issuer's `iss` and
`auth.resource.id` as the OAuth resource and token audience. The server verifies
tokens using that issuer's public keys. The manifest grants the trust entry the
merchant's owner role, so constrain each client's permissions at your issuer.
See [trusted issuers](auth.md#trusted-issuers) and [route permissions](api/routes.md).

For the catalog read below, register a confidential client for `client_credentials`
with `openrails:merchant` scope and `merchant:catalog:read` permission. If your app
uses AuthKit, add these resources and clients to its `AuthorizationServer`
configuration, where `issuerConfig` is your app's `authkit.Config`. Use the same
`clientSecret` from your backend's secret store to create the hex SHA-256 digest.

```go
secretHash := sha256.Sum256([]byte(clientSecret))
permissions := []string{string(billing.MerchantCatalogRead)}
issuerConfig.AuthorizationServer = authkit.AuthorizationServerConfig{
	Resources: []authkit.ResourceServerConfig{{
		ID:          "http://localhost:3053",
		Scopes:      []string{"openrails:merchant"},
		Permissions: permissions,
	}},
	Clients: []authkit.OAuthClientConfig{{
		ID:           "myapp-backend",
		SecretSHA256: hex.EncodeToString(secretHash[:]),
		Resources:    []string{"http://localhost:3053"},
		Permissions:  permissions,
		GrantTypes:   []authkit.OAuthGrantType{authkit.GrantClientCredentials},
	}},
}
```

The full example has both backend and browser OAuth clients. Its Compose files
use the older v0.259.0 server and catalog images. See [the standalone
example](../examples/standalone) and [its AuthKit setup](../examples/standalone/main.go).

## Backend

For a Go backend, install the client from your application directory.

```sh
go get github.com/open-rails/openrails@v0.282.0
```

Import `github.com/open-rails/openrails` and `github.com/open-rails/openrails/billing`.
Pass an application token callback as shown below, or set `RemoteCredential.APIKey`
to a merchant API key with the same permission. The credential must be bound to
your merchant. The `Resource` field is the audience, and the URL passed to
`NewRemoteMerchant` is the service origin without `/v1`.

```go
const serverURL = "http://localhost:3053"

token, err := openrails.NewClientCredentialsTokenProvider(openrails.ClientCredentialsConfig{
	TokenURL:     "http://localhost:8080/oauth2/token",
	ClientID:     "myapp-backend",
	ClientSecret: clientSecret,
	Resource:     serverURL,
})
if err != nil {
	return err
}
shop, err := openrails.NewRemoteMerchant(serverURL,
	openrails.RemoteCredential{Token: token},
	openrails.RemoteMerchantOptions{Timeout: 10 * time.Second})
if err != nil {
	return err
}
products, err := shop.ListProducts(ctx, billing.ProductListParams{})
if err != nil {
	return err
}
log.Printf("OpenRails returned %d products", len(products.Items))
```

The token callback requests `openrails:merchant` by default and caches the
token until shortly before expiry. `ListProducts` is the first network call
and tests the token, merchant binding and catalog permission together.
A 200 with an empty `data` list is valid before a catalog is applied.

To make the same check from another stack, send a bearer access token from the
issuer's `client_credentials` exchange, or a merchant API key, in the header.

```sh
curl --fail-with-body --include \
  -H "Authorization: Bearer $OPENRAILS_MERCHANT_TOKEN" \
  http://localhost:3053/v1/catalog/products
```

Check the `OpenRails-Bound-Merchant` header for your merchant UUID. If you know
the UUID ahead of time, use `NewRemote` with a credential filed under that UUID,
or send the `OpenRails-Expected-Merchant` header over HTTP. This verifies the
binding before an operation, including a first write. To diagnose a failed read,
check the issuer and audience for a 401, the merchant binding and permission for
a 403, and `route_groups.catalog` for a 404. See [merchant
binding](auth.md#merchant-binding) and [API errors](api/errors.md).

**Cache entitlement checks.** A client using a merchant API key caches
`CheckEntitlements` by default. It keeps each allowed key's answer, never a
denial: a check is answered from the cache only when every requested key is
allowed there, and otherwise asks OpenRails for the whole set. A newly held key
is therefore visible at once. A change to a held key, such as its seats or a
revocation, can take up to `MaxAge` (2 minutes by default,
`DefaultEntitlementCheckMaxAge`) unless `CheckEntitlementsLive` sees it, and so
can an API key permission change. An allowed answer also expires at the next
scheduled access change, such as a rental's end.

`CheckEntitlementsLive` always asks OpenRails. Its answer replaces allowed
keys' entries and drops denied ones, and an ordinary check already in flight
cannot restore a dropped entry. In process, only that client's own memory is
affected: other processes keep their entries for up to `MaxAge`. With Redis,
every replica sees the drop once it is written. The write has 250 ms; the live
check waits for it no longer than its own deadline. If Redis fails, replicas
may serve the old answer for up to `MaxAge`.

Configure the cache on `RemoteMerchantOptions.EntitlementCheckCache` (or the
same field on `RemoteOptions`); `Disabled: true` makes every check ask
OpenRails:

```go
client, err := openrails.NewRemoteMerchant(billingURL,
    openrails.RemoteCredential{APIKey: apiKey},
    openrails.RemoteMerchantOptions{
        EntitlementCheckCache: openrails.EntitlementCheckCacheOptions{
            MaxAge: 30 * time.Second, // or Disabled: true
            Redis:  redisClient,      // optional, shared by replicas
        },
    })
```

Entries live in bounded process memory. If you supply your own go-redis client
as `Redis`, replicas using the same OpenRails endpoint, merchant, API key and
`MaxAge` share them. Your backend owns that Redis client and its connection
lifecycle. Use a trusted Redis 6.2+ instance with synchronized clocks across
its failover nodes. Set short socket timeouts and `ContextTimeoutEnabled` on
the Redis client so timed-out lookups release connections promptly. A client
created with `NewRemoteMerchant` learns its merchant in an authenticated
response before it can fill or reuse entries. Prefix or historical requests,
calls with request options and token-backed credentials always ask OpenRails,
as does any check when Redis fails. See
[entitlement windows](entitlements_timeline.md#reading-access).

**Deploy the server before the SDK.** The cache sends `include_validity`,
which an older server refuses: every exact-key check from an API-key client,
`CheckEntitlementsLive` included, then fails with 400 `unknown_field`. Until
the server is upgraded, set `Disabled: true`.

**Usage limits.** `openrails.NewGate(client, openrails.GateConfig{})` admits each
request in your process against holds of the customer's capacity and reports what
it used on the gate's lease ([the gate](usage-billing.md#the-gate)). A warm
decision makes no network call, so it continues while OpenRails is unreachable; a
cold one waits for one `SyncLeases` call, or is `openrails.ErrUnavailable`. A
process alone keeps the gate in memory, and `GateConfig.Outbox` keeps its captured
usage across a crash; with `GateConfig.Redis`, your own Redis, the processes
sharing it are one gate. Nothing else is configured: hold sizes and timing follow
traffic, and the merchant declares only limits, products and prices.

## Browser

Give the browser a short-lived `openrails:self` token. In the AuthKit example,
a public OAuth client trades the user's app session for a DPoP-bound token.
Register the browser origin, permit the resource and `openrails:self` scope,
and use `auth.resourceFetch` for the billing requests. The billing UI base URL
includes `/v1`. Keep merchant credentials on the backend. See [browser
client setup](../examples/standalone/web/src/clients.ts) and [frontend
authentication](frontend.md#authentication).

For an application with a signing key registered at OpenRails, you can also use
`Client.CustomerAssertion` and have the browser redeem it for a customer token.
See [merchant binding](auth.md#merchant-binding).

## Contacts, receipts and webhooks

- Send customer contacts through SCIM or verified contact claims in tokens. See
  [customer contacts](customer-contacts.md).
- Use `Client.ListEvents` or `GET /v1/app/events` with `merchant:events:read` to
  drive your app's receipts. See [events](events.md).
- Point each PSP directly to your billing service at
  `/v1/webhooks/{rail}/{account_id}`. See [provider
  webhooks](api/conventions.md#provider-webhooks) and [local
  webhooks](dev/local-webhooks.md).
- For an off-site checkout, see [payment pages](payment-page.md).
