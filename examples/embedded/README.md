# Embedded example

The root README's program: a creator site on one Go server. Users sign in with
AuthKit, buy CSS courses one at a time or as a bundle (or rent one for 3
days), and join a channel membership, monthly or yearly. The walkthrough is
the root README's [Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | AuthKit, the OpenRails client (`newBilling`) and the mounted routes |
| `content.go` | the gate: `/api/courses/:course` and `/api/members/qa` serve a customer who holds the entitlement, and answer anyone else `402` with the page where they can buy it |
| `web/` | the React app: the course and Q&A pages fetch that content with `auth.authFetch`; `/courses/:course/buy` and `/join` sell with billing-ui |
| `catalog.yaml`, `merchant.example.yaml` | the catalog and the merchant |

AuthKit's token rides `fetch`, not page loads, so content is an API the pages
fetch. Media can't carry it either (`<video src>` sends no token): real video
would play from a short-lived signed URL that the gated route returns.

## Run it

```sh
cp merchant.example.yaml merchant.yaml    # your NMI sandbox gateway's IDs and keys
(cd web && pnpm install && pnpm build)    # the React app, into web/dist
DATABASE_URL=postgres://… go run .        # http://localhost:8080
```

## Tests

`go test -tags integration .` with `OPENRAILS_E2E_DSN` naming a PostgreSQL 18
server. Each run creates and drops its own database, and NMI is
`openrailstest/nmimock`.

- `TestGatedContent` drives the routes billing-ui calls. Signed out, or signed
  in without access, is `402`. A course bought alone, a rental, the bundle and
  a membership each admit their content and nothing else.
- `TestBrowser` serves the same app and runs `web/e2e` in Chromium: sign in
  with auth-ui, pay in billing-ui's `CheckoutModal`, land back on the content.
  It skips until `web/` is installed and built, and Playwright needs its
  browser (`pnpm exec playwright install chromium`).
