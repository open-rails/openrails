# Standalone example

The [embedded example](../embedded)'s course site with OpenRails run as its
own server: the `openrails` server from its release image, with PostgreSQL 18,
Redis and an SMTP server. The app keeps its AuthKit, which is now also the
OAuth issuer the server trusts. The walkthrough is the root README's
"Running in Standalone mode"; the app code they share is
[Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | the app's AuthKit (users verify their email; it mints the browser's and the backend's tokens for the server and pushes users to it over SCIM) and the billing client, `NewRemote` |
| `content.go`, `web/src/pages.tsx`, `catalog.yaml`, `media/` | the embedded example's, unchanged |
| `web/src/clients.ts` | the browser's clients: AuthKit's, which trades the session for a DPoP-bound token to the server, and billing-ui's, which calls the server directly with it |
| `openrails/config.yaml` | the server's configuration: database, Redis, its own AuthKit, the trusted issuer, route groups, SMTP |
| `openrails/merchant.example.yaml` | the merchant the server serves and its NMI PSP; copy it to `merchant.yaml` |
| `compose.yaml` | the stack: PostgreSQL, Redis, Mailpit, the server, a one-shot `apply-catalog`, and the app |
| `compose.e2e.yaml`, `e2e-seed.mjs` | the same stack with NMI played by the server's fake gateway, running `web/e2e` in a browser |
| `Dockerfile`, `postgres-init.sql` | the app's image; the app's database beside the server's |

## Run it

```sh
cp openrails/merchant.example.yaml openrails/merchant.yaml   # your NMI sandbox gateway's IDs and keys
OPENRAILS_CLIENT_SECRET=$(openssl rand -hex 32) docker compose up -d --build
```

The store is http://localhost:8080, the server http://localhost:3053 and the
inbox (verification codes, receipts) http://localhost:8025.
`docker compose down -v` removes it all.

The server image is `docker.io/openrails/openrails:v0.235.0`, the first
release with route groups and the catalog filters this app reads. Until it
is published, build that tag from this checkout:
`docker build -t docker.io/openrails/openrails:v0.235.0 ../..`.

Without Docker, the release binary runs the same files: with PostgreSQL,
Redis and an SMTP server of your own, set `DB_URL`, `REDIS_ADDR`,
`EMAIL_SMTP_HOST` and `AUTH_KEYS_PATH` (each overrides its key in
`config.yaml`), then

```sh
openrails run-server --config openrails/config.yaml --merchant-manifest openrails/merchant.yaml
openrails apply-catalog --config openrails/config.yaml --merchant-manifest openrails/merchant.yaml --merchant onlydemo --file catalog.yaml
(cd web && pnpm install && pnpm build)
DATABASE_URL=postgres://… OPENRAILS_CLIENT_SECRET=… EMAIL_SMTP_HOST=… go run .
```

## Tests

`go test -tags integration .` with `OPENRAILS_E2E_DSN` naming a PostgreSQL 18
server, and `OPENRAILS_E2E_REDIS` a Redis if you want one. Each test runs
the `openrails` binary (`OPENRAILS_BIN`, else built from `../../server`)
with `openrails/config.yaml` and `merchant.example.yaml`, only their ports
changed (and without Redis when none is named), applies the catalog with
`apply-catalog`, and plays NMI with `openrailstest/nmimock` and SMTP with
`smtptest`.

- `TestGatedContent`, `TestCourseList`, `TestMediaURLs`: the embedded
  example's, with each purchase made at the server with a DPoP-bound token,
  as billing-ui does.
- `TestServerSetup`: a user who signs up and proves their email reaches the
  server over SCIM with it; the backend's read-only token reads them and is
  refused a write; their purchase emails a receipt; a signed NMI webhook is
  accepted and a forged one refused.
- `TestBrowser`: `web/e2e` in Chromium against the app and the server.

The whole stack from its images, with the browser tests:

```sh
cp openrails/merchant.example.yaml openrails/merchant.yaml   # its placeholder keys suit the fake gateway
(cd web && pnpm install)
OPENRAILS_CLIENT_SECRET=$(openssl rand -hex 32) docker compose -f compose.yaml -f compose.e2e.yaml run --rm e2e
docker compose -f compose.yaml -f compose.e2e.yaml down -v
```
