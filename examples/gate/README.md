# Gate

An API whose requests count toward usage limits, decided in process by
`openrails.Gate` (`main.go`). OpenRails runs elsewhere (hosted or
standalone); the API holds a key with `merchant:usage:manage`.

```sh
openrails catalog apply catalog.yaml   # or Client.ApplyCatalog
OPENRAILS_URL=https://api.openrails.dev OPENRAILS_API_KEY=… go run .
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
| shutdown | `gate.Close`: sends captured usage and returns the holds |

`REDIS_URL` makes every replica one gate: a customer one replica served is
warm on all of them. Without Redis, usage captured but not yet sent is lost
if the process crashes, and never billed; `GateConfig.Outbox` keeps it.
