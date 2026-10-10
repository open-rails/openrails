# Embedded example

The root README's program: a creator site on one Go server. Users sign in with
AuthKit, buy video courses one at a time or as a bundle (or rent one for 3
days), or join the channel membership, monthly or yearly, which unlocks every
video, members-only ones included. The walkthrough is
the root README's [Sell Your Content](../../README.md#sell-your-content).

| File | What it is |
|---|---|
| `main.go` | AuthKit, the OpenRails client (`newBilling`) and the mounted routes |
| `content.go` | the app's API: `/api/courses` lists the courses with prices and what the user owns (one product read and one entitlement read per page); `/api/courses/:course` answers whoever holds a key that unlocks the video (its course's, or the membership) its signed URL, and anyone else `402` with those keys and the buy page; `/media/*path` serves signed, unexpired URLs |
| `web/` | the React app, under one `BillingProvider`: the store (a billing-ui `<BuyButton>` per price), each course's player, and its buy page, where `<Offers>` sells everything that unlocks it; the app never handles a checkout session. `web/src/clients.ts` builds the auth and billing clients, the one page file the [standalone](../standalone) and [hosted](../hosted) versions change |
| `media/` | the courses' sample videos |
| `catalog.yaml`, `merchant.example.yaml` | the catalog and the merchant |

AuthKit's token rides `fetch`, not page loads or `<video src>`, so pages fetch
the API with `auth.authFetch` and play a short-lived signed URL. In production,
sign with an S3/R2 presigned GET, a CloudFront signed URL or ContentKit's media
tokens. In a real app, content and its previews come from ContentKit, whose
paywall names the entitlement to sell, and `<Offers>` takes it from there.

## Run it

```sh
cp merchant.example.yaml merchant.yaml    # your NMI sandbox gateway's IDs and keys
(cd web && pnpm install && pnpm build)    # the React app, into web/dist
DATABASE_URL=postgres://… go run .        # http://localhost:8080 (MEDIA_KEY signs video URLs)
```

## Tests

`go test -tags integration .` with `OPENRAILS_E2E_DSN` naming a PostgreSQL 18
server. Each run creates and drops its own database, and NMI is
`openrailstest/nmimock`.

- `TestCourseList` pages the courses and counts queries: one product read and
  one entitlement read per page, none of the latter signed out.
- `TestGatedContent` buys through the routes billing-ui calls: a course, a
  rental and the bundle unlock their courses, the membership every video, and
  the signed URL serves the video, ranges included.
- `TestMediaURLs` refuses a tampered, expired or unsigned media URL.
- `TestBrowser` serves the same app and runs `web/e2e` in Chromium: buy from the
  store or a course's buy page (the course, the bundle or the membership), sign
  in with auth-ui, pay, and the video plays.
  It skips until `web/` is installed and built, and Playwright needs its
  browser (`pnpm exec playwright install chromium`).
