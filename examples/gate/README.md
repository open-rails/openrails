# Gate

An API whose requests count toward usage limits, decided in process by
`openrails.Gate` (`main.go`). OpenRails runs elsewhere, hosted or
standalone.

- `OPENRAILS_URL` is the merchant's API URL: on the hosted platform,
  **API URL** under **Integration** in the console; standalone, your server's
  base URL.
- `OPENRAILS_API_KEY` is a merchant API key (hosted: **Settings → API keys**)
  whose role holds `merchant:usage:manage` and `merchant:usage:read`.
- A customer is admitted only while it holds a product giving the limits
  (`starter`) and has a balance to pay for its usage (`usage` prices each
  unit): grant `starter` and `usage`, and fund its balance
  ([prepaid credits](../../docs/prepaid-credits.md)), before calling the API
  for it.

```sh
openrails catalog apply catalog.yaml   # or Client.ApplyCatalog
OPENRAILS_URL=https://… OPENRAILS_API_KEY=… go run .
curl -X POST -H "X-Customer-ID: $CUSTOMER" 'localhost:8080/v1/work?units=40'
curl -H "X-Customer-ID: $CUSTOMER" localhost:8080/v1/limits
```

| Step | Call |
|---|---|
| admit the estimate | `gate.Admit`: in process while the customer's hold covers it; one `SyncLeases` round trip when it does not |
| refused | `Ticket.Refusal()`: `limit_reached` with `resets_at`, or a money deny code; answered 429 |
| OpenRails unreachable | warm decisions continue; a cold one is `openrails.ErrUnavailable` (503) |
| work done | `Ticket.Capture` with the units used, sent on the gate's next sync |
| work failed | `Ticket.Release` |
| shutdown | after the server drains, `gate.Close` sends captured usage and returns the holds |

`REDIS_URL` makes every replica one gate: a customer one replica served is
warm on all of them. Without Redis, usage captured but not yet sent is lost
if the process crashes, and never billed; `GateConfig.Outbox` keeps it. An
outbox's `Put` stores all of its items or none, and returns an error only when
it stored nothing.
