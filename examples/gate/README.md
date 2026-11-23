# Gate

An API whose requests count toward usage limits, admitted in process by
`openrails.Gate` (`main.go`) against a hosted or standalone OpenRails
([client: the Gate](../../docs/client.md#limits-admissions-and-the-gate)).
`catalog.yaml` declares the limits, a priced product and a $10 credit pack.

- `OPENRAILS_URL`, the merchant's API URL; `OPENRAILS_MERCHANT_ID`, its id;
  `OPENRAILS_API_KEY`, a key holding `merchant:usage:manage` and `merchant:usage:read`.
- A customer is admitted while it holds `starter` (the limits) and `usage` (the
  price per unit) and has a balance: grant both and sell it `credits-10`.
- `REDIS_URL` makes every replica one gate. Without Redis or an outbox, usage
  captured but not yet sent is lost if the process crashes.

```sh
openrails catalog apply catalog.yaml   # or Client.ApplyCatalog
OPENRAILS_URL=https://… OPENRAILS_MERCHANT_ID=… OPENRAILS_API_KEY=… go run .
curl -X POST -H "X-Customer-ID: $CUSTOMER" 'localhost:8080/v1/work?units=40'
curl -H "X-Customer-ID: $CUSTOMER" localhost:8080/v1/limits
```
