package converge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/open-rails/openrails/internal/billing/decline"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// Payment health findings (#1118): decline and rebill-failure spikes per PSP
// account and owner, system errors, and decline codes no table maps. They
// read the #1116 measures, so an alert and the console's health page agree on
// every number. Each is a standing finding: it stays open while its condition
// holds (one notification per episode) and resolves when it no longer does.
const (
	findingNewCardDeclineSpike = "life.payments.new_card_decline_spike"
	findingRebillFailureSpike  = "life.payments.rebill_failure_spike"
	findingSystemErrors        = "life.payments.system_errors"
	findingDeclineUnmapped     = "life.decline.unmapped"
)

// Starting thresholds; each rate needs its minimum volume first.
const (
	spikeWindow        = 24 * time.Hour
	baselineWindow     = 28 * 24 * time.Hour
	spikeRise          = 0.10 // ten percentage points above the baseline
	spikeMinVolume     = 50
	rebillFailureLimit = 0.25
	systemErrorWindow  = time.Hour
	systemErrorShare   = 0.02
	systemErrorMin     = 20
	unmappedLookback   = 30 * 24 * time.Hour
)

// credentialCodes are NMI's answers about our own account (410 invalid
// merchant configuration, 411 inactive merchant account): every charge fails
// until the operator fixes it, so one is critical at once.
var credentialCodes = []string{"410", "411"}

var newCardFilters = map[string][]string{"kind": {"verify", "initial"}, "card_entry": {"new"}}

// paymentHealthFindings evaluates the merchant's payment health at now.
func (p *lifePass) paymentHealthFindings(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	var out []ConvergeFinding
	for _, step := range []func(context.Context, time.Time) ([]ConvergeFinding, error){
		p.newCardSpikes, p.rebillSpikes, p.systemErrors, p.unmappedDeclines,
	} {
		fs, err := step(ctx, now)
		if err != nil {
			return nil, fmt.Errorf("life: payment health: %w", err)
		}
		out = append(out, fs...)
	}
	return out, nil
}

// healthKey is one PSP account and owner.
type healthKey struct{ account, owner string }

func (k healthKey) subject() string { return "psp:" + k.account + ":owner:" + k.owner }

// rateCell is a group's volume and failure rate.
type rateCell struct {
	volume float64
	rate   float64
}

// rates runs measures [volume, rate] by rail_account and owner over [from, to).
func (p *lifePass) rates(ctx context.Context, volume, rate string, filters map[string][]string, from, to, now time.Time) (map[healthKey]rateCell, error) {
	rows, err := p.metricRows(ctx, metrics.Query{Measures: []string{volume, rate}, By: []string{"rail_account", "owner"}, Filters: filters}, from, to, now)
	if err != nil {
		return nil, err
	}
	out := map[healthKey]rateCell{}
	for _, r := range rows {
		out[healthKey{str(r["rail_account"]), str(r["owner"])}] = rateCell{volume: num(r[volume]), rate: num(r[rate])}
	}
	return out, nil
}

// newCardSpikes: the trailing-24h new-card decline rate at least ten points
// above its 28-day baseline, both with the minimum volume.
func (p *lifePass) newCardSpikes(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	recent, err := p.rates(ctx, "attempts", "attempt_failure_rate", newCardFilters, now.Add(-spikeWindow), now, now)
	if err != nil {
		return nil, err
	}
	baseline, err := p.rates(ctx, "attempts", "attempt_failure_rate", newCardFilters, now.Add(-spikeWindow-baselineWindow), now.Add(-spikeWindow), now)
	if err != nil {
		return nil, err
	}
	var out []ConvergeFinding
	for _, k := range sortedKeys(recent) {
		cur, base := recent[k], baseline[k]
		if cur.volume < spikeMinVolume || base.volume < spikeMinVolume || cur.rate < base.rate+spikeRise {
			continue
		}
		out = append(out, spikeFinding(findingNewCardDeclineSpike, k, cur, base, fmt.Sprintf(
			"New-card declines on %s (%s) rose to %.1f%% over the last 24 hours against %.1f%% over the 28 days before: look for a checkout regression or card testing.",
			k.account, k.owner, cur.rate*100, base.rate*100)))
	}
	return out, nil
}

// rebillSpikes: the trailing-24h rebill first-attempt failure rate at least
// ten points above its baseline, or above 25%, over the minimum cycles.
func (p *lifePass) rebillSpikes(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	recent, err := p.rates(ctx, "rebill_first_failures", "rebill_first_failure_rate", nil, now.Add(-spikeWindow), now, now)
	if err != nil {
		return nil, err
	}
	baseline, err := p.rates(ctx, "rebill_first_failures", "rebill_first_failure_rate", nil, now.Add(-spikeWindow-baselineWindow), now.Add(-spikeWindow), now)
	if err != nil {
		return nil, err
	}
	var out []ConvergeFinding
	for _, k := range sortedKeys(recent) {
		cur, base := attemptedCycles(recent[k]), attemptedCycles(baseline[k])
		if cur.volume < spikeMinVolume {
			continue
		}
		risen := base.volume >= spikeMinVolume && cur.rate >= base.rate+spikeRise
		if !risen && cur.rate <= rebillFailureLimit {
			continue
		}
		out = append(out, spikeFinding(findingRebillFailureSpike, k, cur, base, fmt.Sprintf(
			"Rebill first attempts on %s (%s) failed %.1f%% of the time over the last 24 hours (baseline %.1f%%): check the PSP and the decline reasons on the payment health page.",
			k.account, k.owner, cur.rate*100, base.rate*100)))
	}
	return out, nil
}

// attemptedCycles turns first failures and their rate into the cycles with a
// first outcome, the rate's denominator.
func attemptedCycles(c rateCell) rateCell {
	if c.rate <= 0 {
		return rateCell{}
	}
	return rateCell{volume: math.Round(c.volume / c.rate), rate: c.rate}
}

func spikeFinding(kind string, k healthKey, cur, base rateCell, action string) ConvergeFinding {
	return ConvergeFinding{
		Type: kind, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
		SubjectKey: k.subject(), Provider: "self",
		Evidence: map[string]any{
			"rail_account": k.account, "owner": k.owner,
			"window_hours": int(spikeWindow.Hours()), "count": int64(cur.volume), "rate": cur.rate,
			"baseline_days": int(baselineWindow.Hours() / 24), "baseline_count": int64(base.volume), "baseline_rate": base.rate,
		},
		RecommendedAction: action,
	}
}

// systemErrors: system errors above 2% of the last hour's attempts (at least
// 20), or any answer about our own credentials, which is critical.
func (p *lifePass) systemErrors(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	from := now.Add(-systemErrorWindow)
	rows, err := p.metricRows(ctx, metrics.Query{Measures: []string{"attempts"}, By: []string{"rail_account", "owner", "category"}}, from, now, now)
	if err != nil {
		return nil, err
	}
	total, broken := map[healthKey]float64{}, map[healthKey]float64{}
	for _, r := range rows {
		k := healthKey{str(r["rail_account"]), str(r["owner"])}
		total[k] += num(r["attempts"])
		if r["category"] == string(decline.SystemError) {
			broken[k] += num(r["attempts"])
		}
	}
	rows, err = p.metricRows(ctx, metrics.Query{Measures: []string{"attempts"}, By: []string{"rail_account", "owner", "response_code"},
		Filters: map[string][]string{"rail": {"nmi"}, "response_code": credentialCodes}}, from, now, now)
	if err != nil {
		return nil, err
	}
	credentials := map[healthKey][]string{}
	for _, r := range rows {
		k := healthKey{str(r["rail_account"]), str(r["owner"])}
		credentials[k] = append(credentials[k], str(r["response_code"]))
	}
	var out []ConvergeFinding
	for _, k := range sortedKeys(total) {
		share := broken[k] / total[k]
		codes := credentials[k]
		if len(codes) == 0 && (total[k] < systemErrorMin || share <= systemErrorShare) {
			continue
		}
		f := ConvergeFinding{
			Type: findingSystemErrors, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
			SubjectKey: k.subject(), Provider: "self",
			Evidence: map[string]any{"rail_account": k.account, "owner": k.owner, "window_minutes": int(systemErrorWindow.Minutes()),
				"count": int64(total[k]), "system_errors": int64(broken[k]), "share": share},
			RecommendedAction: fmt.Sprintf("%.0f of the last hour's %.0f attempts on %s (%s) failed on a system error: check the PSP's status and our connection to it.",
				broken[k], total[k], k.account, k.owner),
		}
		if len(codes) > 0 {
			sort.Strings(codes)
			f.Severity = SeverityCritical
			f.Evidence["credential_codes"] = codes
			f.RecommendedAction = fmt.Sprintf("%s answered %v in the last hour: our merchant account or credentials are refused, so every charge on it fails. Fix the account with the PSP now.",
				k.account, codes)
		}
		out = append(out, f)
	}
	return out, nil
}

// unmappedDeclines: a decline code no table maps, per rail and code. It
// resolves once the code is mapped or ages out of the lookback.
func (p *lifePass) unmappedDeclines(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	rows, err := p.metricRows(ctx, metrics.Query{Measures: []string{"failed_attempts"}, By: []string{"rail", "response_code"}}, now.Add(-unmappedLookback), now, now)
	if err != nil {
		return nil, err
	}
	var out []ConvergeFinding
	for _, r := range rows {
		rail, code := str(r["rail"]), str(r["response_code"])
		if code == "" || num(r["failed_attempts"]) == 0 || !decline.Classify(rail, code).NeedsMapping() {
			continue
		}
		out = append(out, ConvergeFinding{
			Type: findingDeclineUnmapped, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityMedium,
			SubjectKey: "decline:" + rail + ":" + code, Provider: "self",
			Evidence: map[string]any{"rail": rail, "code": code, "declines": int64(num(r["failed_attempts"])), "lookback_days": int(unmappedLookback.Hours() / 24)},
			RecommendedAction: fmt.Sprintf("%s decline code %q is in no table: it is retried as an ordinary decline and reads as \"unknown\". Map it in internal/billing/decline.",
				rail, code),
		})
	}
	return out, nil
}

// metricRows runs one metrics query over [from, to) and returns its rows by
// column name.
func (p *lifePass) metricRows(ctx context.Context, q metrics.Query, from, to, now time.Time) ([]map[string]any, error) {
	q.Range = &metrics.QueryRange{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339)}
	plan, verr := metrics.ValidateAt(&q, now)
	if verr != nil {
		return nil, verr
	}
	res, err := metrics.NewService(p.e.DB).Execute(ctx, plan)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		m := make(map[string]any, len(row))
		for i, c := range res.Columns {
			m[c.Name] = row[i]
		}
		out = append(out, m)
	}
	return out, nil
}

func sortedKeys[V any](m map[healthKey]V) []healthKey {
	keys := make([]healthKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].owner < keys[j].owner
	})
	return keys
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
