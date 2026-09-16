# Browser authentication qualification

This fixture drives actual AuthKit login and `/delegated/token`, browser
WebCrypto, CORS/preflight and the real OpenRails customer route over disposable
PostgreSQL/Redis. It also checks attached HttpOnly cookies on same-origin,
same-site sibling and cross-site POSTs, with and without JSON bodies.

```sh
pnpm --dir tests/browser install --frozen-lockfile
pnpm --dir tests/browser exec playwright install chromium
GOMAXPROCS=2 GOWORK=off go test -p 1 -race -tags=integration,browser \
  ./internal/integrationharness -run '^TestBrowserAuthenticationContract$' -count=1 -v
```

The Go test owns the issuer/resource/cookie fixture and creates only temporary
credentials and disposable stores. Never set test database or Redis overrides
to a retained application store. No payment provider is contacted.

The merchant notification console contract serves the actual compiled admin and
real API. Build `web/admin` first, then run the same browser dependency setup and
`go test -tags=integration,browser ./internal/integrationharness -run '^TestMerchantNotificationsBrowser$' -count=1 -v`.
It verifies Notifications navigation, absence of the deferred rule editor, and
creation of an encrypted webhook destination. `OPENRAILS_BROWSER_SCREENSHOT`
optionally records the resulting page.
