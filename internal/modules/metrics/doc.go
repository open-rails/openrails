// Package metrics is the merchant analytics engine: an in-code registry of
// measures and dimensions compiles a JSON query to parameterized SQL per source
// family, so no client string ever becomes SQL text.
//
// Routes: GET /v1/metrics/schema dumps the registry (every measure's
// description, formula, class and allowed dimensions; dimension values; the
// query shape; examples, caveats and limits) as a document an agent or
// dashboard can carry in its prompt. POST /v1/metrics/query runs one query;
// both need merchant:metrics:read (or its root twin), are scoped to the
// credential's merchant and answer aggregates only, never entity rows.
//
// A query takes measures and range (required), by, grain (auto picks day, week
// or month from the range), filters (OR within a dimension, AND across),
// order, limit and compare (previous_period or previous_year); any other key
// is refused. range is
// {from, to} as inclusive UTC days or an RFC 3339 half-open interval, or
// {last: "7d"} ending today on the deployment's clock. product_id and price_id
// filters take the catalog's typed ids; a bare UUID is invalid_filter_value.
// The answer is {grain, range, columns, rows, compare_range, compare_rows}:
// time series zero-fill every bucket, money cells are decimal strings of
// native units (MoneyCell; an average is rounded to whole units), and money
// never sums across currencies (currency becomes an implicit group-by). Limits:
// 1 to 1000 rows, at most 400 time buckets.
//
// Validation answers 400 metrics_query_invalid with every error at once in
// metadata.errors, each with its code (unknown_measure, ...), param,
// did_you_mean and the valid names for that slot, so one corrected retry
// succeeds; a removed measure's error names its replacement. Each measure is
// in the business or the ops group and says which way a change is good.
// Funnels (attempts, purchases, rebills, payments) are one measure with an
// outcome or kind dimension and rates over its slices. nmi_history_authorizations
// comes from nmi_history_months and cannot separate first one-off sales from
// retried rebills.
package metrics
