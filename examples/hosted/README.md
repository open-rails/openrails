# Hosted example

The course store on the hosted platform: a remote client with the console's
API key, customer tokens for the browser, the catalog command and entitlement
checks ([client: credentials](../../docs/client.md#credentials)).

```sh
(cd web && pnpm install && pnpm build)
OPENRAILS_URL=… OPENRAILS_RESOURCE=… OPENRAILS_SERVICE_TOKEN=… OPENRAILS_DIRECTORY=… \
  go run . catalog apply catalog.yaml
DATABASE_URL=postgres://… OPENRAILS_URL=… OPENRAILS_RESOURCE=… OPENRAILS_SERVICE_TOKEN=… \
  OPENRAILS_DIRECTORY=… go run .
```

`OPENRAILS_DIRECTORY` is the platform's SCIM directory for this app's users,
reached with the same key.
