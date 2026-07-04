# Metrics for LLMs — wiring any agent to the OpenRails query API

Any LLM agent with a merchant API key can answer analytics questions against OpenRails
directly. The API was designed for this (#733): the schema endpoint IS the machine-readable
documentation, validation errors are corrective instructions returned all at once, and
results are token-lean tables. Two endpoints, no SDK.

The hosted `POST /v1/merchant/metrics/ask` endpoint (#756, the admin console's Ask panel)
is exactly this pattern operated server-side — same schema-as-context, same query tool,
same validation loop. An MCP server wrapper is a trivial future step: the tool definition
below is its spec.

## Auth

- A merchant **API key** sent as `Authorization: Bearer <key>`, carrying the
  `merchant:metrics:read` permission. Mint one from the console (Settings) or the
  merchant API.
- Everything is scoped to the key's merchant at the database layer (RLS) — there is no
  cross-merchant read, and the metrics API serves **aggregates only** (never entity rows).
- Bases: standalone `https://<host>/v1`, embedded typically `https://<host>/billing/v1`.

## Step 1 — fetch the schema (the context document)

```
GET /v1/merchant/metrics/schema
```

Self-describing registry, designed to ride in an LLM context: every measure carries a
one-line description + formula + allowed dimensions; dimensions carry their enum values;
`query_shape` states the body grammar; `examples` pairs natural-language intents with
correct query JSON (imitate them); `derived` lists formulas to compose client-side
(e.g. `arpu = mrr / subscriptions[active]`); `caveats` states what the data is NOT
(e.g. revenue is gross of processor fees). Put the whole JSON in the system prompt.

## Step 2 — run queries

```
POST /v1/merchant/metrics/query
{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}
```

Response: `{columns, rows, grain, range, compare_range?, compare_rows?}` — tabular, one
row per group-key combination. Time series **zero-fill** every bucket in range (a zero is
data, not an omission). Money values are **micros** (1,000,000 micros = 1 currency unit);
money never sums across currencies — `currency` appears as an implicit group-by when
ambiguous.

## Error semantics (the self-correction loop)

Validation failures are `400` with **every** error at once — one retry fixes everything:

```json
{"error":{"code":"metrics_query_invalid","metadata":{"errors":[
  {"code":"unknown_measure","param":"measures[0]",
   "message":"unknown measure \"cancelations\": did you mean \"cancellations\"?",
   "did_you_mean":"cancellations","valid":["..."]}
]}}}
```

Unknown names get did-you-mean + the full valid list for that slot; an illegal
dimension-for-measure names that measure's supported dims; unknown JSON **keys** in the
body are 400s too (a typo'd `"filter"` fails loudly, never silently ignored). Feed the
`errors` array back to the model verbatim and retry.

## Copy-paste tool definition

Works in any agent framework (Anthropic tool use shown; the same name/description/schema
drops into OpenAI functions, MCP, etc.). Execute it by POSTing the arguments verbatim to
`/v1/merchant/metrics/query`; on 400, return the response's `errors` array as the tool
error; put the `/schema` JSON in the system prompt for vocabulary.

```json
{
  "name": "run_metrics_query",
  "description": "Run one OpenRails metrics query (the POST /v1/merchant/metrics/query body) against the merchant's own data. Measures, dimensions, grains and worked examples are defined in the metrics schema document (GET /v1/merchant/metrics/schema) — use only names from it. Returns tabular {columns, rows}; time series are zero-filled over the whole range (a zero means genuinely zero). Validation errors come back all at once with corrective context (did-you-mean, valid lists).",
  "input_schema": {
    "type": "object",
    "properties": {
      "measures": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Measure names from the metrics schema."},
      "by": {"type": "array", "items": {"type": "string"}, "description": "Group-by dimensions; include \"time\" for a time series."},
      "grain": {"type": "string", "enum": ["day", "week", "month", "quarter", "year"], "description": "Time bucket size; required when \"by\" includes \"time\"."},
      "range": {
        "type": "object",
        "properties": {
          "from": {"type": "string", "description": "YYYY-MM-DD (inclusive UTC day) or RFC3339."},
          "to": {"type": "string", "description": "YYYY-MM-DD (inclusive UTC day) or RFC3339."},
          "last": {"type": "string", "description": "Relative trailing window ending today: <N>d|<N>w|<N>m|<N>y, e.g. \"7d\". Mutually exclusive with from/to."}
        },
        "additionalProperties": false
      },
      "filters": {"type": "object", "additionalProperties": {"type": "array", "items": {"type": "string"}}, "description": "Dimension name -> allowed values (OR within a dimension, AND across)."},
      "order": {"type": "array", "items": {"type": "object", "properties": {"measure": {"type": "string"}, "dimension": {"type": "string"}, "dir": {"type": "string", "enum": ["asc", "desc"]}}, "additionalProperties": false}},
      "limit": {"type": "integer", "minimum": 1},
      "compare": {"type": "string", "enum": ["previous_period"], "description": "Also return the same query over the immediately preceding period."}
    },
    "required": ["measures", "range"],
    "additionalProperties": false
  }
}
```

Suggested system-prompt rules (what the hosted /ask uses): every number in an answer must
come from a tool result — never improvise; if the schema can't answer, say what's missing;
state money in whole currency units (results are micros); prefer one query with a group-by
over many single queries.
