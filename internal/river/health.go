package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
)

// Health is OpenRails' jobs as River's own tables in one schema hold them.
type Health struct {
	// CheckedAt is the database's clock.
	CheckedAt time.Time
	// LeaderElectedAt is when the schema's River leader, which schedules
	// periodic passes and rescues crashed jobs, was elected; nil when none
	// holds the lease.
	LeaderElectedAt *time.Time
	Kinds           []billing.WorkerHealth
}

// ReadHealth reads every OpenRails job kind from River's tables in schema:
// its jobs running past their timeout, waiting to retry or discarded, a
// periodic kind's last completion and the newest error. It writes nothing.
// River's tables are not in the sqlc schema, so the SQL is here.
func ReadHealth(ctx context.Context, pool *pgxpool.Pool, schema string) (Health, error) {
	if pool == nil {
		return Health{}, fmt.Errorf("job health: no pool")
	}
	every := map[string]time.Duration{}
	for _, p := range Periodics {
		every[p.Args.Kind()] = p.Every
	}
	names := Kinds()
	timeouts := make([]float64, len(names))
	periodic := make([]bool, len(names))
	for i, kind := range names {
		timeouts[i] = Timeout(kind).Seconds()
		_, periodic[i] = every[kind]
	}
	jobs, leader := pgx.Identifier{schema, "river_job"}.Sanitize(), pgx.Identifier{schema, "river_leader"}.Sanitize()
	rows, err := pool.Query(ctx, `
WITH k AS (SELECT * FROM unnest($1::text[], $2::float8[], $3::bool[]) AS k(kind, timeout_secs, periodic)),
unfinished AS (
    SELECT j.kind,
           count(*) FILTER (WHERE j.state = 'running' AND j.attempted_at < now() - make_interval(secs => k.timeout_secs)) AS overrunning,
           count(*) FILTER (WHERE j.state = 'retryable') AS retryable,
           count(*) FILTER (WHERE j.state = 'discarded') AS discarded
    FROM `+jobs+` j JOIN k ON k.kind = j.kind
    WHERE j.state IN ('running', 'retryable', 'discarded')
    GROUP BY j.kind
)
SELECT k.kind, coalesce(u.overrunning, 0), coalesce(u.retryable, 0), coalesce(u.discarded, 0),
       CASE WHEN k.periodic THEN (SELECT max(j.finalized_at) FROM `+jobs+` j WHERE j.kind = k.kind AND j.state = 'completed') END,
       (SELECT j.errors[array_upper(j.errors, 1)] ->> 'error' FROM `+jobs+` j
         WHERE j.kind = k.kind AND j.state IN ('retryable', 'discarded') AND cardinality(j.errors) > 0
         ORDER BY j.attempted_at DESC NULLS LAST LIMIT 1),
       now(),
       (SELECT l.elected_at FROM `+leader+` l WHERE l.expires_at > now() LIMIT 1)
FROM k LEFT JOIN unfinished u ON u.kind = k.kind
ORDER BY k.kind`, names, timeouts, periodic)
	if err != nil {
		return Health{}, fmt.Errorf("job health: read %s: %w", jobs, err)
	}
	defer rows.Close()
	var h Health
	for rows.Next() {
		var k billing.WorkerHealth
		if err := rows.Scan(&k.Kind, &k.Overrunning, &k.Retryable, &k.Discarded, &k.LastCompletedAt, &k.LastError, &h.CheckedAt, &h.LeaderElectedAt); err != nil {
			return Health{}, fmt.Errorf("job health: %w", err)
		}
		k.TimeoutSeconds = int64(Timeout(k.Kind) / time.Second)
		if d, ok := every[k.Kind]; ok {
			s := int64(d / time.Second)
			k.PeriodSeconds = &s
		}
		h.Kinds = append(h.Kinds, k)
	}
	if err := rows.Err(); err != nil {
		return Health{}, fmt.Errorf("job health: %w", err)
	}
	return h, nil
}
