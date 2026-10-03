package converge

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// Payment health findings (#1118): decline and rebill-failure spikes per PSP
// account and owner, system errors, decline codes no table maps, and PSPs
// whose webhooks went silent (#1112). They
// read the #1116 measures, so an alert and the console's health page agree on
// every number. Each is a standing finding: it stays open while its condition
// holds (one notification per episode) and resolves when it no longer does.
// Rates are compared as integer ratios of counts.
const (
	findingNewCardDeclineSpike = "life.payments.new_card_decline_spike"
	findingRebillFailureSpike  = "life.payments.rebill_failure_spike"
	findingSystemErrors        = "life.payments.system_errors"
	findingDeclineUnmapped     = "life.decline.unmapped"
	findingWebhookSilence      = "life.webhooks.silent"
)

// Starting thresholds; each rate needs its minimum volume first.
const (
	spikeWindow          = 24 * time.Hour
	baselineWindow       = 28 * 24 * time.Hour
	spikeRisePoints      = 10 // percentage points above the baseline
	spikeMinVolume       = 50
	rebillFailurePercent = 25
	systemErrorWindow    = time.Hour
	systemErrorPercent   = 2
	systemErrorMin       = 20
	unmappedLookback     = 30 * 24 * time.Hour
	// A PSP whose provider charges arrived by webhook over the baseline, with
	// none by webhook in the silence window while pulls found some, is silent.
	silenceWindow      = 24 * time.Hour
	silenceBaselineMin = 10
	silenceMinPulled   = 3
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
		p.newCardSpikes, p.rebillSpikes, p.systemErrors, p.unmappedDeclines, p.webhookSilence,
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

// share is failed out of total.
type share struct{ failed, total int64 }

// atLeastPoints reports whether s is at least points percentage points above base.
func (s share) atLeastPoints(base share, points int64) bool {
	return 100*s.failed*base.total >= (100*base.failed+points*base.total)*s.total
}

// over reports whether s is above percent.
func (s share) over(percent int64) bool { return 100*s.failed > percent*s.total }

// String is the share as a percentage with one decimal.
func (s share) String() string {
	if s.total == 0 {
		return "n/a"
	}
	tenths := (1000*s.failed + s.total/2) / s.total
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

// shares runs [failed, total] by rail_account and owner over [from, to).
func (p *lifePass) shares(ctx context.Context, failed, total string, filters map[string][]string, from, to, now time.Time) (map[healthKey]share, error) {
	rows, err := p.metricRows(ctx, metrics.Query{Measures: []string{failed, total}, By: []string{"rail_account", "owner"}, Filters: filters}, from, to, now)
	if err != nil {
		return nil, err
	}
	out := map[healthKey]share{}
	for _, r := range rows {
		out[healthKey{str(r["rail_account"]), str(r["owner"])}] = share{failed: count(r[failed]), total: count(r[total])}
	}
	return out, nil
}

// spikes compares the last day against the 28 days before it.
func (p *lifePass) spikes(ctx context.Context, failed, total string, filters map[string][]string, now time.Time) (recent, baseline map[healthKey]share, err error) {
	if recent, err = p.shares(ctx, failed, total, filters, now.Add(-spikeWindow), now, now); err != nil {
		return nil, nil, err
	}
	baseline, err = p.shares(ctx, failed, total, filters, now.Add(-spikeWindow-baselineWindow), now.Add(-spikeWindow), now)
	return recent, baseline, err
}

// newCardSpikes: the trailing-24h new-card decline rate at least ten points
// above its 28-day baseline, both with the minimum volume.
func (p *lifePass) newCardSpikes(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	recent, baseline, err := p.spikes(ctx, "failed_attempts", "attempts", newCardFilters, now)
	if err != nil {
		return nil, err
	}
	var out []ConvergeFinding
	for _, k := range sortedKeys(recent) {
		cur, base := recent[k], baseline[k]
		if cur.total < spikeMinVolume || base.total < spikeMinVolume || !cur.atLeastPoints(base, spikeRisePoints) {
			continue
		}
		out = append(out, spikeFinding(findingNewCardDeclineSpike, k, cur, base, fmt.Sprintf(
			"New-card declines on %s (%s) rose to %s over the last 24 hours against %s over the 28 days before: look for a checkout regression or card testing.",
			k.account, k.owner, cur, base)))
	}
	return out, nil
}

// rebillSpikes: the trailing-24h rebill first-attempt failure rate at least
// ten points above its baseline, or above 25%, over the minimum cycles.
func (p *lifePass) rebillSpikes(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	recent, baseline, err := p.spikes(ctx, "rebill_first_failures", "rebills_attempted", nil, now)
	if err != nil {
		return nil, err
	}
	var out []ConvergeFinding
	for _, k := range sortedKeys(recent) {
		cur, base := recent[k], baseline[k]
		if cur.total < spikeMinVolume {
			continue
		}
		risen := base.total >= spikeMinVolume && cur.atLeastPoints(base, spikeRisePoints)
		if !risen && !cur.over(rebillFailurePercent) {
			continue
		}
		out = append(out, spikeFinding(findingRebillFailureSpike, k, cur, base, fmt.Sprintf(
			"Rebill first attempts on %s (%s) failed %s of the time over the last 24 hours (baseline %s): check the PSP and the decline reasons on the payment health page.",
			k.account, k.owner, cur, base)))
	}
	return out, nil
}

func spikeFinding(kind string, k healthKey, cur, base share, action string) ConvergeFinding {
	return ConvergeFinding{
		Type: kind, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
		SubjectKey: k.subject(), Provider: "self",
		Evidence: map[string]any{
			"rail_account": k.account, "owner": k.owner, "window_hours": int(spikeWindow.Hours()),
			"failed": cur.failed, "count": cur.total, "rate": cur.String(),
			"baseline_days": int(baselineWindow.Hours()) / 24, "baseline_failed": base.failed, "baseline_count": base.total, "baseline_rate": base.String(),
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
	errs := map[healthKey]share{}
	for _, r := range rows {
		k := healthKey{str(r["rail_account"]), str(r["owner"])}
		s := errs[k]
		s.total += count(r["attempts"])
		if r["category"] == string(decline.SystemError) {
			s.failed += count(r["attempts"])
		}
		errs[k] = s
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
	for _, k := range sortedKeys(errs) {
		s, codes := errs[k], credentials[k]
		if len(codes) == 0 && (s.total < systemErrorMin || !s.over(systemErrorPercent)) {
			continue
		}
		f := ConvergeFinding{
			Type: findingSystemErrors, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
			SubjectKey: k.subject(), Provider: "self",
			Evidence: map[string]any{"rail_account": k.account, "owner": k.owner, "window_minutes": int(systemErrorWindow.Minutes()),
				"count": s.total, "system_errors": s.failed, "share": s.String()},
			RecommendedAction: fmt.Sprintf("%d of the last hour's %d attempts on %s (%s) failed on a system error: check the PSP's status and our connection to it.",
				s.failed, s.total, k.account, k.owner),
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
		rail, code, n := str(r["rail"]), str(r["response_code"]), count(r["failed_attempts"])
		if code == "" || n == 0 || !decline.Classify(rail, code).NeedsMapping() {
			continue
		}
		out = append(out, ConvergeFinding{
			Type: findingDeclineUnmapped, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityMedium,
			SubjectKey: "decline:" + rail + ":" + code, Provider: "self",
			Evidence: map[string]any{"rail": rail, "code": code, "declines": n, "lookback_days": int(unmappedLookback.Hours()) / 24},
			RecommendedAction: fmt.Sprintf("%s decline code %q is in no table: it is retried as an ordinary decline and reads as \"unknown\". Map it in internal/decline.",
				rail, code),
		})
	}
	return out, nil
}

// webhookSilence: a PSP whose provider-scheduled charges normally arrive by
// webhook has sent none for a day while pulls still find its charges. One per
// PSP account; it resolves when webhooks resume.
func (p *lifePass) webhookSilence(ctx context.Context, now time.Time) ([]ConvergeFinding, error) {
	observed := func(from, to time.Time) (map[string]map[string]int64, error) {
		rows, err := p.metricRows(ctx, metrics.Query{Measures: []string{"attempts"}, By: []string{"rail_account", "observed_via"},
			Filters: map[string][]string{"source": {"provider_schedule"}}}, from, to, now)
		if err != nil {
			return nil, err
		}
		out := map[string]map[string]int64{}
		for _, r := range rows {
			account := str(r["rail_account"])
			if out[account] == nil {
				out[account] = map[string]int64{}
			}
			out[account][str(r["observed_via"])] += count(r["attempts"])
		}
		return out, nil
	}
	recent, err := observed(now.Add(-silenceWindow), now)
	if err != nil {
		return nil, err
	}
	baseline, err := observed(now.Add(-silenceWindow-baselineWindow), now.Add(-silenceWindow))
	if err != nil {
		return nil, err
	}
	accounts := make([]string, 0, len(recent))
	for account := range recent {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	var out []ConvergeFinding
	for _, account := range accounts {
		cur, base := recent[account], baseline[account]
		if base["webhook"] < silenceBaselineMin || cur["webhook"] > 0 || cur["pull"] < silenceMinPulled {
			continue
		}
		out = append(out, ConvergeFinding{
			Type: findingWebhookSilence, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
			SubjectKey: "psp:" + account, Provider: "self",
			Evidence: map[string]any{"rail_account": account, "window_hours": int(silenceWindow.Hours()),
				"pulled": cur["pull"], "baseline_days": int(baselineWindow.Hours()) / 24, "baseline_webhook": base["webhook"]},
			RecommendedAction: fmt.Sprintf("%s sent no webhooks in the last 24 hours, but pulls found %d of its charges; it had sent %d in the 28 days before. Its charges and declines reach OpenRails late: check the webhook registration and signing key at the gateway.",
				account, cur["pull"], base["webhook"]),
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

// count is a count measure's cell.
func count(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}
