-- Stores rates read at fetched_at, each pair replaced in place unless a
-- later read already replaced it.
-- name: PutFXRates :exec
INSERT INTO billing.fx_rates (from_currency, to_currency, rate, as_of, fetched_at)
SELECT r.from_currency, r.to_currency, r.rate, r.as_of, sqlc.arg(fetched_at)::timestamptz
FROM unnest(sqlc.arg(from_currencies)::text[], sqlc.arg(to_currencies)::text[], sqlc.arg(rates)::float8[], sqlc.arg(as_ofs)::timestamptz[]) AS r(from_currency, to_currency, rate, as_of)
ON CONFLICT (from_currency, to_currency) DO UPDATE SET rate = EXCLUDED.rate, as_of = EXCLUDED.as_of, fetched_at = EXCLUDED.fetched_at
WHERE billing.fx_rates.fetched_at <= EXCLUDED.fetched_at;

-- name: GetFXRate :one
SELECT rate, as_of, fetched_at FROM billing.fx_rates
WHERE from_currency = sqlc.arg(from_currency)::text AND to_currency = sqlc.arg(to_currency)::text;
