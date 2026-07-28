# Metrics for LLMs — wiring any agent to the OpenRails query API

Any LLM agent or dashboard with a merchant API key can answer analytics questions against
OpenRails directly (#733): the schema endpoint IS the machine-readable documentation,
validation errors are corrective instructions returned all at once, and results are
token-lean tables. No SDK.

## Auth

- Bearer merchant **API key** (`Authorization: Bearer <key>`) carrying the
  `merchant:metrics:read` permission; mint one from the console (Settings) or the API.
- Everything is scoped to the key's merchant at the database layer (RLS); the API serves
  **aggregates only**, never entity rows.
- Prefix: standalone `/v1`, embedded typically `/billing/v1`.

## Endpoints

| Method + path | Purpose |
|---|---|
| `GET /v1/merchant/metrics/schema` | The registry dump — the LLM context document |
| `POST /v1/merchant/metrics/query` | Run one composable query |
| `POST /v1/merchant/metrics/ask` | Hosted Q&A (#756): `{"question":"..."}`, LLM runs `/query` server-side |

**Schema first.** The `/schema` JSON is designed to ride in a system prompt: every measure
carries description + formula + allowed dims; dimensions carry enum values; `query_shape`
states the body grammar; `examples` pairs intents with correct query JSON (imitate them);
`derived` lists client-side formulas (e.g. arpu = mrr / active subscriptions); `caveats`
states what the data is NOT; `limits` states the caps. Use only names from it.

## Query shape

```json
POST /v1/merchant/metrics/query
{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}
```

Valid body keys (anything else is a 400): `measures` (required), `by`, `grain`, `range`
(required), `filters`, `order`, `limit`, `compare`.

- `range`: `{"from","to"}` as `YYYY-MM-DD` (inclusive UTC days) or RFC3339 `[from,to)`,
  **or** `{"last":"7d"}` (`Nd|Nw|Nm|Ny`, trailing window ending today; exclusive with from/to).
- `by`: group-by dimensions; include `"time"` for a time series (`grain` then required:
  `day|week|month|quarter|year`).
- `filters`: `{dimension: [values...]}` — OR within a dimension, AND across.
- `order`: `[{"measure"|"dimension": name, "dir":"asc"|"desc"}]`.
- `compare`: `"previous_period"` — same query over the immediately preceding period.

Response: `{grain, range, columns, rows, compare_range?, compare_rows?}` — one row per
group-key combination. Time series **zero-fill** every bucket (a zero is data, not an
omission). Money is **micros** (1,000,000 = 1 currency unit) and never sums across
currencies — `currency` becomes an implicit group-by when ambiguous.

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
tool error, and put the `/schema` JSON in the system prompt. Suggested rules (what the
hosted `/ask` uses): every number in an answer must come from a tool result; if the schema
can't answer, say what's missing; state money in whole currency units.

`/ask` is the same loop operated server-side; its response carries the answer plus the
verbatim result of every executed query as evidence. Because aggregate results are sent to
the LLM provider, it is consent-gated (`LLM_ASK_ENABLED=true` + `LLM_API_KEY`; otherwise
501) and rate-limited per merchant (429 + `Retry-After`).
