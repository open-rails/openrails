# Standalone example

The [embedded example](../embedded)'s course site with OpenRails run as its
own server: the `openrails` server from its release image, with PostgreSQL 18,
Redis and an SMTP server. The app keeps its AuthKit, which is now also the
OAuth issuer the server trusts. The walkthrough is the root README's
"Running in Standalone mode"; the app code they share is
[Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | the app's AuthKit (users verify their email; it mints the browser's and the backend's tokens for the server and pushes users to the server's AuthKit over SCIM) and the billing client, `NewRemoteMerchant` |
| `content.go`, `web/src/pages.tsx`, `catalog.yaml`, `media/` | the embedded example's, unchanged |
| `web/src/clients.ts` | the browser's clients: AuthKit's, which trades the session for a DPoP-bound token to the server, and billing-ui's, which calls the server directly with it |
| `openrails/config.yaml` | the server's configuration: database, Redis, its own AuthKit (a resource server), route groups, SMTP |
| `openrails/merchant.example.yaml` | the merchant the server serves, the app's AuthKit as its trusted issuer, and its NMI PSP; copy it to `merchant.yaml` |
| `compose.yaml` | the stack: PostgreSQL, Redis, Mailpit, the server, a one-shot `catalog apply`, and the app |
| `Dockerfile`, `postgres-init.sql` | the app's image; the app's database beside the server's |

## Run it

```sh
cp openrails/merchant.example.yaml openrails/merchant.yaml   # your NMI sandbox gateway's IDs and keys
OPENRAILS_CLIENT_SECRET=$(openssl rand -hex 32) docker compose up -d --build
```

The store is http://localhost:8080, the server http://localhost:3053 and the
inbox (verification codes, receipts) http://localhost:8025.
`docker compose down -v` removes it all.

The server image is the release `compose.yaml` pins; to run this checkout
instead, build it: `docker build -t docker.io/openrails/openrails:dev ../..` and
point `compose.yaml` at that tag.

The example keeps fixed, separate directory IDs for the app and the billing
platform. For your deployment generate each ID once, set the app's
`AUTH_DEFAULT_TENANT_ID` and the server's `auth.default_tenant_id`, and preserve
them across restarts. `openrails migrate up` uses the server's configured ID
when adopting an existing single-directory AuthKit database.

Without Docker, the release binary runs the same files: with PostgreSQL,
Redis and an SMTP server of your own, set `DB_URL`, `REDIS_ADDR`,
`EMAIL_SMTP_HOST` and `AUTH_KEYS_PATH` (each overrides its key in
`config.yaml`), then

```sh
openrails run-server --config openrails/config.yaml --merchant-manifest openrails/merchant.yaml
openrails catalog apply catalog.yaml --config openrails/config.yaml --merchant-manifest openrails/merchant.yaml --merchant onlydemo
(cd web && pnpm install && pnpm build)
DATABASE_URL=postgres://… OPENRAILS_CLIENT_SECRET=… EMAIL_SMTP_HOST=… go run .
```
