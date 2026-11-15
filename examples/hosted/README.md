# Hosted example

The [embedded example](../embedded)'s course site on the hosted OpenRails
platform: OpenRails runs there, and this app is one merchant's. The
walkthrough is the root README's "Using our Hosted OpenRails Platform"; the
app code they share is [Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | the app's AuthKit (users verify their email; it mints the browser's tokens for the platform and pushes users to it over SCIM with the service token) and the billing client, `NewRemoteMerchant` with the service token |
| `catalog.go` | the app's catalog command, `go run . catalog apply catalog.yaml`: `Client.ApplyCatalog` with the service token; starting the app never applies it |
| `content.go`, `web/src/pages.tsx`, `catalog.yaml`, `media/` | the embedded example's, unchanged |
| `web/src/clients.ts` | the browser's clients: AuthKit's, which trades the session for a DPoP-bound token to the platform, and billing-ui's, which calls the platform directly with it |

## Run it

Connect the app to your merchant first (the root README's walkthrough): the
platform's API and resource identifier, this app's AuthKit registered
as the merchant's signing application, a service token bound to that
registration, and the platform's directory for its users. Then

```sh
(cd web && pnpm install && pnpm build)
DATABASE_URL=postgres://… EMAIL_SMTP_HOST=… \
OPENRAILS_URL=https://openrails.dev OPENRAILS_RESOURCE=https://openrails.dev \
OPENRAILS_SERVICE_TOKEN=openrails_st_… OPENRAILS_DIRECTORY=https://openrails.dev/directory/scim/v2 \
PUBLIC_URL=https://courses.example go run . catalog apply catalog.yaml   # once, and whenever it changes
… go run .
```

AuthKit keeps its signing key in `AUTH_KEYS_PATH` (default `.dev/auth`): the
platform trusts that key, so keep it across restarts.
