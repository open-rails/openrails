# Hosted example

The [embedded example](../embedded)'s course site on the hosted OpenRails
platform: OpenRails runs there, and this app is one merchant's. The
walkthrough is the root README's "Using our Hosted OpenRails Platform"; the
app code they share is [Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | the app's AuthKit (users verify their email; it mints the browser's tokens for the platform and pushes users to it over SCIM with the service token) and the billing client, `NewRemote` with the service token, which applies `catalog.yaml` at boot |
| `content.go`, `web/src/pages.tsx`, `catalog.yaml`, `media/` | the embedded example's, unchanged |
| `web/src/clients.ts` | the browser's clients: AuthKit's, which trades the session for a DPoP-bound token to the platform, and billing-ui's, which calls the merchant's API host directly with it |

## Run it

Connect the app to your merchant first (the root README's walkthrough): its
API host, the platform's resource identifier, this app's AuthKit registered
as the merchant's signing application, a service token bound to that
registration, and the platform's directory for its users. Then

```sh
(cd web && pnpm install && pnpm build)
DATABASE_URL=postgres://… EMAIL_SMTP_HOST=… \
OPENRAILS_API_HOST=https://api.onlydemo.openrails.dev OPENRAILS_RESOURCE=https://openrails.dev \
OPENRAILS_SERVICE_TOKEN=openrails_st_… OPENRAILS_DIRECTORY=https://openrails.dev/directory/scim/v2 \
PUBLIC_URL=https://courses.example go run .
```

AuthKit keeps its signing key in `AUTH_KEYS_PATH` (default `.dev/auth`): the
platform trusts that key, so keep it across restarts.

## Tests

`go test -tags integration .` against a platform this app is connected to,
with `OPENRAILS_E2E_DSN` naming a PostgreSQL 18 server for the app's own
database and `OPENRAILS_HOSTED_API_HOST`, `_RESOURCE`, `_SERVICE_TOKEN`, `_DIRECTORY`,
`_APP_URL` (the issuer the platform registered), `_KEYS` (its signing key),
`_MAILPIT` and `_SMTP_PORT` (the inbox the app's and the platform's mail
reach). The tests are the standalone example's, run at the platform:
`TestGatedContent`, `TestCourseList`, `TestMediaURLs`, `TestPlatformSetup`
(SCIM, receipts, webhooks) and `TestBrowser`.
