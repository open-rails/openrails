# Connect a merchant backend

Standalone OpenRails and SaaS expose the same merchant client. Each backend credential is bound to one merchant. Keep the credential and any customer assertion signing key on your backend.

## 1. Grant the backend a merchant role

Create a merchant API key in the merchant's AuthKit group, or register your OAuth issuer's application with a role in that group. The SaaS console manages these registrations; standalone hosts can use AuthKit's credential administration or its Go client. Billing routes do not mint backend credentials.

Choose a role for the operations your backend needs. `viewer` can read customer billing, catalog, merchant configuration, metrics and usage. `support` can read and manage customer billing. `owner` holds the merchant namespace. Custom roles can combine individual permissions:

| Purpose | Permission |
|---|---|
| Read catalog | `merchant:catalog:read` |
| Change catalog | `merchant:catalog:manage` |
| Read merchant configuration and PSPs | `merchant:config:read` |
| Change configuration, export/import billing archives | `merchant:config:manage` |
| Inspect usage and running charges | `merchant:usage:read` |
| Record usage, reserve spend, start or stop running charges | `merchant:usage:manage` |
| Check entitlements | `merchant:entitlements:read` |
| Create orders | `merchant:orders:manage` |

Grant read and manage together when the application needs both. A read grant does not authorize mutations. Human sessions cannot invoke application-only routes, even if their role holds the permission.

## 2. Construct the client

An API key requires no token exchange:

```go
client, err := openrails.NewRemoteMerchant(
    "https://billing.example.com",
    openrails.RemoteCredential{APIKey: os.Getenv("OPENRAILS_API_KEY")},
    openrails.RemoteMerchantOptions{Timeout: 10 * time.Second},
)
```

The optional `RemoteMerchantOptions` also accepts an `HTTPClient` for custom TLS and connection pooling. Its `Timeout` bounds the complete request, including token acquisition, without extending the caller's deadline. Multi-merchant backends use `NewRemote` with an individual credential per merchant and the same transport/timeout options.

For OAuth, request `openrails:merchant` for the deployment's configured `auth.resource.id` from your trusted issuer. The helper implements the standard [client-credentials grant](https://www.rfc-editor.org/rfc/rfc6749.html#section-4.4), `client_secret_basic` authentication, and the [resource parameter](https://www.rfc-editor.org/rfc/rfc8707.html). It caches tokens until shortly before expiry and obtains a new token when needed; client credentials do not require a refresh token.

```go
token, err := openrails.NewClientCredentialsTokenProvider(openrails.ClientCredentialsConfig{
    TokenURL:     "https://issuer.example/oauth2/token",
    ClientID:     os.Getenv("OAUTH_CLIENT_ID"),
    ClientSecret: os.Getenv("OAUTH_CLIENT_SECRET"),
    Resource:     "https://billing.example.com",
})
if err != nil { return err }
client, err := openrails.NewRemoteMerchant(
    "https://billing.example.com",
    openrails.RemoteCredential{Token: token},
    openrails.RemoteMerchantOptions{Timeout: 10 * time.Second},
)
```

The default scope is `openrails:merchant`. Token acquisition uses the caller's context, with a 30-second default HTTP client, and concurrent callers share the cached token. A custom token-endpoint `HTTPClient` is configured separately from the billing transport. The helper does not follow token-endpoint redirects, log credentials, or persist tokens. Tokens without an expiry are not cached. Other issuer authentication methods can use an application's existing token provider through the same callback.

## 3. Check configuration and access

`client.GetPublicConfig(ctx)` reads `GET /v1/config`, including mounted route groups and deployment features, without invoking or sending the credential. `client.GetAdminAccess(ctx)` authenticates the credential and reports:

- `admin`, `catalog`, `merchant_config`: `none`, `read`, or `update`.
- `metrics`: whether metrics are readable.
- `application_permissions`: permissions this application holds for mounted `/v1/app` operations. Root grants are expressed as the corresponding merchant permission. Human callers receive an empty list.
- `scope_id`: the merchant's own AuthKit group.

The `catalog` and `merchant_config` fields previously were booleans; consumers must compare the level explicitly rather than treating any nonempty string as access.
These console section levels require the read permission: a manage-only grant
does not expose a section, and `update` means both read and manage are held.

The access route is mounted when any staff or programmatic group is enabled, including a programmatic-only deployment. It is an advisory snapshot: each operation authenticates and authorizes its own request. Public feature flags distinguish deployment availability from credential authority.

## 4. Optionally authorize customer-token signing

Backend billing credentials and customer identity assertions are independent. Register the backend's signing application and public keys in the merchant's group, then configure `RemoteCredential.Assertion` with its issuer, key ID, private signer and token URL. `CustomerToken` exchanges an assertion for an `openrails:self` token; `CustomerAssertion` only signs locally.

An API key alone cannot sign customer tokens. Registering an application only for customer assertions grants no backend billing role. Do not add an owner role merely to enable customer sign-in. See [customer tokens](standalone-integration.md#trusted-issuers-staff-machines-and-customers) and [merchant provisioning](merchant-provisioning.md).

Standalone reads AuthKit's customer directory. OpenRails' `/v1/app/scim/v2` routes and provisioning-token CRUD are therefore absent. Embedded hosts that maintain OpenRails' pushed customer directory can enable the programmatic group; `capabilities.features.customer_provisioning` reports that consumer and its credential management. Provision the standalone directory through AuthKit's own mechanism.
