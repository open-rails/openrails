# Hosted example

The course store on the hosted platform. The backend calls the platform with
a merchant API key and mints a customer token for each signed-in user; the
browser's billing-ui buys and manages billing with it. `POST /api/checkout`
answers a hosted pay URL from `CreateOrder` instead; `CheckEntitlements` gates
the courses ([client: credentials](../../docs/client.md#credentials)).

1. In the console, create the merchant, its PSP and an API key; note the API URL and the merchant id.
2. Make a signing key and register its public half under **Integration →
   Customer tokens**, with `PUBLIC_URL` as the issuer and a key id:
   `openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out signing.pem && openssl pkey -in signing.pem -pubout`.

```sh
export OPENRAILS_URL=… OPENRAILS_MERCHANT_ID=… OPENRAILS_API_KEY=… \
  OPENRAILS_TOKEN_URL=… OPENRAILS_KEY_ID=… OPENRAILS_SIGNING_KEY=signing.pem
(cd web && pnpm install && pnpm build)
go run . catalog apply catalog.yaml
DATABASE_URL=postgres://… go run .   # http://localhost:8080; mail on EMAIL_SMTP_HOST:1025
```
