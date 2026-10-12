# Metrics for LLMs — wiring any agent to the OpenRails query API

Any LLM agent or dashboard with a merchant credential can answer analytics questions against
OpenRails directly: the schema endpoint IS the machine-readable documentation,
validation errors are corrective instructions returned all at once, and results are
token-lean tables. No SDK.

## Auth

- A bearer credential (`Authorization: Bearer <token>`) holding
  `billing.MerchantMetricsRead` at the merchant, or `billing.RootMetricsRead`:
  on the standalone server a client-credentials access token from the
  merchant's trusted issuer granting it, or an API key whose role holds it.
- Every query is scoped to the credential's merchant (an explicit merchant predicate); the API serves
  **aggregates only**, never entity rows.
- Prefix: standalone `/v1`, embedded typically `/billing/v1`.

## Endpoints

| Method + path | Purpose |
|---|---|
| `GET /v1/metrics/schema` | The registry dump — the LLM context document |
| `POST /v1/metrics/query` | Run one composable query |

**Schema first.** The `/schema` JSON is designed to ride in a system prompt: every measure
carries description + formula + allowed dims; dimensions carry enum values; `query_shape`
states the body grammar; `examples` pairs intents with correct query JSON (imitate them);
`caveats` states what the data is NOT; `limits` states the caps. Use only names from it.
A measure's `class` is how it aggregates: `additive`, `distinct`, `ratio`, `difference`
(one measure minus another, e.g. `net_new_subscriptions`) or `snapshot`.

## Query shape

```json
POST /v1/metrics/query
{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}
```

Valid body keys (anything else is a 400): `measures` (required), `by`, `grain`, `range`
(required), `filters`, `order`, `limit`, `compare`.

- `range`: `{"from","to"}` as `YYYY-MM-DD` (inclusive UTC days) or RFC3339 `[from,to)`,
  **or** `{"last":"7d"}` (`Nd|Nw|Nm|Ny`, trailing window ending today on the
  deployment's clock; exclusive with from/to). `{"last":"1d"}` with `compare` is today
  against yesterday.
- `by`: group-by dimensions; include `"time"` for a time series (`grain` then required:
  `day|week|month|quarter|year`).
- `filters`: `{dimension: [values...]}` — OR within a dimension, AND across.
  `product_id` / `price_id` values (and the `product_id` / `price_id` cells a
  group-by returns) are the catalog's typed ids (`prod_…`, `premium:buy-usd:v1`); a bare
  UUID is an `invalid_filter_value`. `customer` is the customer's plain UUID.
- `order`: `[{"measure"|"dimension": name, "dir":"asc"|"desc"}]`.
- `compare`: `"previous_period"` — same query over the immediately preceding period.

Response: `{grain, range, columns, rows, compare_range?, compare_rows?}` — one row per
group-key combination. Time series **zero-fill** every bucket (a zero is data, not an
omission). Money columns (unit `money`) are decimal strings of exact integer native
units at each currency's registered scale (10^6 for USD/EUR, 10^4 for JPY); a
money-unit ratio (an average) is rounded to whole native units. Counts are JSON
numbers and ratios floats. Money never sums across currencies — `currency`
becomes an implicit group-by when ambiguous.

## Limits

- `limit`: 1..1000 (default 1000 rows).
- Time series: max 400 buckets — shrink the range or coarsen the grain.
- Batch friendliness: prefer one query with a group-by over many single queries.

## Error semantics (the self-correction loop)

Validation failures are `400` with **every** error at once — one retry fixes everything:

```json
{"error":{"code":"metrics_query_invalid","metadata":{"errors":[
  {"code":"unknown_measure","param":"measures[0]",
   "message":"unknown measure \"cancelations\": did you mean \"cancellations\"?",
   "did_you_mean":"cancellations","valid":["..."]}
]}}}
```

Unknown names get did-you-mean + the valid list for that slot; an illegal
dimension-for-measure names the measure's supported dims; unknown body **keys** fail
loudly too. Feed the `errors` array back to the model verbatim and retry.

## Wiring an agent

Define one tool, `run_metrics_query`, whose input schema is the query body above; execute
it by POSTing the arguments verbatim to `/query`, return a 400's `errors` array as the
tool error, and put the `/schema` JSON in the system prompt. Suggested rules: every number in an answer must come from a tool result; if the schema
can't answer, say what's missing; state money in whole currency units.

OpenRails does not provide a built-in AI metrics question endpoint. Hosts may build their own agents over the query and schema API.
