package alerting

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// EvalStats summarizes one merchant's evaluation pass.
type EvalStats struct {
	Evaluated int
	Fired     int
	Cleared   int
	Digests   int
	Errors    int
}

// ArmedMerchantIDs returns every merchant with at least one enabled rule — the
// evaluator scheduler's work set. CROSS-MERCHANT (base pool): work scales with
// rules on file, never with the merchant directory (#719).
func (s *Service) ArmedMerchantIDs(ctx context.Context) ([]uuid.UUID, error) {
	return s.store.listArmedMerchants(ctx)
}

// EvaluateMerchant runs every enabled rule for the merchant in ctx, edge-
// triggering threshold rules and emitting due digests. It pins one RLS-scoped
// connection for the whole pass. One rule's failure is logged and skipped — it
// never aborts the rest.
func (s *Service) EvaluateMerchant(ctx context.Context) (EvalStats, error) {
	now := s.now()
	var stats EvalStats
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rules, err := s.store.listEnabledRules(ctx)
		if err != nil {
			return fmt.Errorf("alerting: list enabled rules: %w", err)
		}
		for _, rule := range rules {
			if e := s.evaluateRule(ctx, rule, now, &stats); e != nil {
				stats.Errors++
				log.WithContext(ctx).WithError(e).
					WithField("rule_id", rule.ID).WithField("template", rule.Template).
					Error("alerting: rule evaluation failed; continuing")
			}
		}
		return nil
	})
	return stats, err
}

func (s *Service) evaluateRule(ctx context.Context, rule Rule, now time.Time, stats *EvalStats) error {
	compiled, _, verr := compileRule(rule.Template, rule.Params)
	if verr != nil {
		return fmt.Errorf("compile stored rule: %s", verr.Error())
	}
	def := templates[rule.Template]

	// Digest cadence gate: skip the query entirely when not yet due.
	if def.digest {
		cadence := def.cadence
		if cadence <= 0 {
			cadence = DigestCadence
		}
		if rule.LastEvaluatedAt != nil && now.Sub(*rule.LastEvaluatedAt) < cadence {
			return nil
		}
	}

	ev := &evalContext{metrics: s.metrics, deps: s.store, now: now}
	outcome, err := compiled.evaluate(ctx, ev)
	if err != nil {
		return err
	}
	stats.Evaluated++

	if def.digest {
		if !outcome.Crossed {
			// Nothing to report: do NOT advance the cadence watermark, so the
			// (cheap) count re-runs next tick until there is something to send.
			return nil
		}
		s.deliverer.dispatch(ctx, rule, s.buildAlert(rule, outcome, now))
		stats.Digests++
		return s.store.touchEvaluated(ctx, rule.ID, now, outcome.Value)
	}

	// Threshold edge-trigger: fire once on crossing, stay quiet while active,
	// record the clear on recrossing.
	firing := rule.FiredAt != nil
	switch {
	case outcome.Crossed && !firing:
		s.deliverer.dispatch(ctx, rule, s.buildAlert(rule, outcome, now))
		stats.Fired++
		return s.store.markFired(ctx, rule.ID, now, now, outcome.Value, outcome.Dims)
	case !outcome.Crossed && firing:
		stats.Cleared++
		return s.store.markCleared(ctx, rule.ID, now, now, outcome.Value)
	default:
		return s.store.touchEvaluated(ctx, rule.ID, now, outcome.Value)
	}
}

func (s *Service) buildAlert(rule Rule, outcome Outcome, now time.Time) Alert {
	return Alert{
		RuleID:        rule.ID,
		RuleName:      rule.Name,
		Template:      rule.Template,
		Severity:      rule.Severity,
		Metric:        outcome.Metric,
		Value:         outcome.Value,
		Threshold:     outcome.Threshold,
		Unit:          outcome.Unit,
		Dimensions:    outcome.Dims,
		Summary:       outcome.Summary,
		DashboardLink: s.dashboardLink(outcome.Metric, outcome.Dims),
		FiredAt:       now,
	}
}
