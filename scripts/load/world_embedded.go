package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-rails/openrails"
)

// runEmbedded is the embedded shape: each gate process runs its own engine
// on one schema, the hot merchant and the first normal one.
func (e *env) runEmbedded(ctx context.Context) *WorldResult {
	plan := e.plan
	w := &WorldResult{Name: "embedded", Mode: "embedded"}
	const schema = "load_embedded"
	if err := e.resetWorld(ctx, schema); err != nil {
		w.fail("reset: %v", err)
		return w
	}
	ms := []MerchantPlan{plan.Hot}
	if len(plan.Normal) > 0 {
		ms = append(ms, plan.Normal[0])
	}
	var slugs []string
	for _, m := range ms {
		slugs = append(slugs, m.Slug)
	}
	rdb, err := openRedis(plan.RedisURL)
	if err != nil {
		w.fail("redis: %v", err)
		return w
	}
	if rdb != nil {
		defer rdb.Close()
	}
	eng, err := openrails.New(ctx, embeddedConfig(schema, schema, slugs), openrails.Deps{Postgres: e.pool, Redis: rdb, ClickHouse: e.ch, FXTransport: fxWire{}, Logger: quietLogger(os.Stderr)})
	if err != nil {
		w.fail("engine: %v", err)
		return w
	}
	defer eng.Close(context.Background())
	admin := map[string]*openrails.Client{}
	seeds := map[string]*seeded{}
	logf("[embedded] seeding")
	for _, m := range ms {
		id, err := eng.DeclaredMerchant(m.Slug)
		if err != nil {
			w.fail("merchant %s: %v", m.Slug, err)
			return w
		}
		admin[m.Slug], _ = eng.Merchant(id)
		s := &seeded{slug: m.Slug, class: classOf(plan, m.Slug), id: id}
		if err := seed(ctx, admin[m.Slug], plan, s, m.Customers, plan.DrainCustomers); err != nil {
			w.fail("seed %s: %v", m.Slug, err)
			return w
		}
		seeds[m.Slug] = s
	}
	f := &fleet{e: e, world: w, ms: ms}
	defer f.killRunning()
	var specs []gateSpec
	for p := range plan.Processes {
		gs := e.gateSpec(p, "embedded", fmt.Sprintf("e%d", p), schema, seeds, ms)
		gs.Results = filepath.Join(e.out, fmt.Sprintf("embedded-gates-%d.result.json", p))
		if err := f.add(gs, fmt.Sprintf("embedded-gates-%d", p)); err != nil {
			w.fail("gate process %d: %v", p, err)
			return w
		}
		specs = append(specs, gs)
	}
	stopSampling := e.sample(ctx, w, schema, f.currentPhase, f.pids)
	f.rates(plan.Hot.Rate)
	f.setPhase("warm")
	sleepCtx(ctx, time.Duration(plan.WarmSeconds)*time.Second)
	f.setPhase("steady")
	f.send(control{Op: "drain", Rate: plan.DrainRate / float64(plan.Processes)})
	sleepCtx(ctx, time.Duration(plan.EmbeddedSteadySeconds)*time.Second)
	f.send(control{Op: "drain", Rate: 0})
	for _, rate := range plan.EmbeddedRamp {
		if ctx.Err() != nil {
			break
		}
		st := e.rampStep(ctx, f, w, schema, seeds[plan.Hot.Slug].id, rate)
		w.Steps = append(w.Steps, st)
		if st.Saturated {
			w.Saturation = fmt.Sprintf("saturated at %d events/s: %v", rate, st.Reasons)
			break
		}
	}
	if w.Saturation == "" && len(plan.EmbeddedRamp) > 0 {
		w.Saturation = fmt.Sprintf("not saturated up to %d events/s", plan.EmbeddedRamp[len(plan.EmbeddedRamp)-1])
	}
	f.endPhase()
	logf("[embedded] stopping gate processes")
	f.stopAll()
	stopSampling()
	w.collect(specs)
	e.checkWorld(ctx, w, schema, seeds, admin)
	return w
}
