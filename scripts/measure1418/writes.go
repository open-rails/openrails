package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgxConn = *pgx.Conn

type writeRow struct {
	Change, Arm string
	N, Failed   int
	P50MS       float64
	P95MS       float64
	MeanMS      float64
	Errors      []string `json:",omitempty"`
}

type candidate struct {
	id, customer, pm uuid.UUID
	tier, cur        int
}

// measureWrites times each kind of subscription change on the full book,
// first with today's triggers, then with billing.settle_mrr writing only the
// facts (no rollups) and the risk queue's triggers off.
func measureWrites(ctx context.Context, pool *pgxpool.Pool, c config, w *world, asof time.Time, rep *report) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	k := c.writeSamples
	rows, err := conn.Query(ctx, `SELECT s.id, s.customer_id, s.payment_method_id, s.price_id FROM billing.subscriptions s
		JOIN billing.subscription_mrr m ON m.merchant_id = s.merchant_id AND m.subscription_id = s.id
		WHERE s.merchant_id = $1 AND s.product_id = $2 AND s.status = 'active' AND m.risk = '{}' AND m.ends_at IS NULL AND m.mrr > 0
		  AND NOT EXISTS (SELECT 1 FROM billing.scheduled_changes x WHERE x.merchant_id = s.merchant_id AND x.subscription_id = s.id AND x.status = 'scheduled')
		  AND NOT EXISTS (SELECT 1 FROM billing.subscriptions a WHERE a.merchant_id = s.merchant_id AND a.customer_id = s.customer_id
		                    AND a.product_id = $3 AND a.status <> 'canceled')
		ORDER BY md5(s.id::text) LIMIT $4`, w.merchant, w.core, w.addon, 2*5*k)
	if err != nil {
		return err
	}
	var cands []candidate
	for rows.Next() {
		var cd candidate
		var price uuid.UUID
		if err := rows.Scan(&cd.id, &cd.customer, &cd.pm, &price); err != nil {
			rows.Close()
			return err
		}
		cd.tier = w.tierOf[price]
		cd.cur = 0
		if w.prices[1][cd.tier] == price {
			cd.cur = 1
		}
		cands = append(cands, cd)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cands) < 10*k {
		k = len(cands) / 10
	}
	logf("write samples: %d per change and arm", k)
	clock := asof.Add(time.Hour)
	next := func() time.Time { clock = clock.Add(time.Second); return clock }
	for arm := range 2 {
		name := "today"
		if arm == 1 {
			name = "facts"
			for _, sql := range []string{settleFactsSQL,
				`ALTER TABLE billing.payment_methods DISABLE TRIGGER payment_methods_track_mrr_risk`,
				`ALTER TABLE billing.money_settings DISABLE TRIGGER money_settings_track_mrr_risk`,
				`ALTER TABLE billing.customer_delinquency DISABLE TRIGGER customer_delinquency_track_mrr_risk`,
				`ALTER TABLE billing.payment_attempts DISABLE TRIGGER payment_attempts_track_mrr_risk`} {
				if _, err := conn.Exec(ctx, sql); err != nil {
					return fmt.Errorf("facts-only writes: %w", err)
				}
			}
		}
		set := cands[arm*5*k : (arm+1)*5*k]
		group := func(i int) []candidate { return set[i*k : (i+1)*k] }
		timed := func(change string, subs []candidate, op func(cd candidate) []timedStmt) {
			row := writeRow{Change: change, Arm: name}
			var durs []time.Duration
			for _, cd := range subs {
				var d time.Duration
				failed := false
				for _, st := range op(cd) {
					t0 := time.Now()
					_, err := conn.Exec(ctx, st.sql, st.args...)
					if st.timed {
						d += time.Since(t0)
					}
					if err != nil {
						failed = true
						if len(row.Errors) < 5 {
							row.Errors = append(row.Errors, err.Error())
						}
						break
					}
				}
				row.N++
				if failed {
					row.Failed++
					continue
				}
				durs = append(durs, d)
			}
			if len(durs) > 0 {
				slices.Sort(durs)
				var sum time.Duration
				for _, d := range durs {
					sum += d
				}
				row.P50MS, row.P95MS, row.MeanMS = ms(durs[len(durs)/2]), ms(durs[len(durs)*95/100]), ms(sum)/float64(len(durs))
			}
			rep.Writes = append(rep.Writes, row)
			logf("write %-28s %-6s n=%d failed=%d p50 %.2f ms p95 %.2f ms mean %.2f ms", change, name, row.N, row.Failed, row.P50MS, row.P95MS, row.MeanMS)
		}
		period := func(t time.Time) (time.Time, time.Time) { return t.Add(-10 * day), t.Add(20 * day) }
		set2 := func(cd candidate, t time.Time, tier int, status string, ps, pe time.Time, cancel, ended any) timedStmt {
			return timedStmt{true, qSet2, []any{w.merchant, cd.id, w.prices[cd.cur][tier], nil, nil, t, status, ps, pe, cancel, ended}}
		}

		timed("create (new)", group(0), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			cust, pm, s := uuid.New(), uuid.New(), uuid.New()
			return []timedStmt{{true, qArrive, []any{w.merchant, cust, w.psp, pm, asof.Year() + 6, s, w.prices[cd.cur][0], w.core, nil, nil, nil, ps, pe, t}}}
		})
		timed("price change (expansion)", group(0), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			to := min(cd.tier+1, 2)
			if to == cd.tier {
				to = cd.tier - 1
			}
			return []timedStmt{set2(cd, t, to, "active", ps, pe, nil, nil)}
		})
		ends := map[uuid.UUID]time.Time{}
		timed("cancel at period end", group(1), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			ends[cd.id] = pe
			return []timedStmt{set2(cd, t, cd.tier, "canceled", ps, pe, "user", pe)}
		})
		timed("resume", group(1), func(cd candidate) []timedStmt {
			t := next()
			ps, _ := period(t)
			return []timedStmt{set2(cd, t, cd.tier, "active", ps, ends[cd.id], nil, nil)}
		})
		timed("churn now (merchant cancel)", group(2), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			return []timedStmt{set2(cd, t, cd.tier, "canceled", ps, pe, "merchant", t)}
		})
		timed("past due (at risk)", group(3), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			return []timedStmt{set2(cd, t, cd.tier, "past_due", ps, pe, nil, nil)}
		})
		timed("recover (risk closed)", group(3), func(cd candidate) []timedStmt {
			t := next()
			ps, pe := period(t)
			return []timedStmt{set2(cd, t, cd.tier, "active", ps, pe, nil, nil)}
		})
		timed("card updated", group(4), func(cd candidate) []timedStmt {
			t := next()
			out := []timedStmt{{true, qCardRenew, []any{w.merchant, cd.pm, 11, asof.Year() + 7, nil, nil, nil, nil, t}}}
			if arm == 0 {
				out = append(out, timedStmt{true, qDrain, []any{w.merchant}})
			}
			return out
		})
		if arm == 0 {
			// openrails.mrr_due at a canceled plan's end; the facts-only cut
			// writes nothing then: its churn was dated at the end when the
			// cancel was recorded.
			timed("churn at period end (mrr_due)", group(4), func(cd candidate) []timedStmt {
				t := next()
				end := t.Add(30 * time.Second)
				return []timedStmt{
					{false, qSet2, []any{w.merchant, cd.id, w.prices[cd.cur][cd.tier], nil, nil, t, "canceled", t.Add(-10 * day), end, "user", end}},
					{true, qEffect, []any{w.merchant, cd.id, nil, end.Add(30 * time.Second)}},
				}
			})
		}
	}
	return nil
}

type timedStmt struct {
	timed bool
	sql   string
	args  []any
}
