package reconcile

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// View is the finding as the admin API and the proposals answer it: its
// parsed recommendation, and a pull.* finding's proposal.
func (rec FindingRecord) View() billing.Finding {
	v := billing.Finding{
		ID: billing.FindingID(rec.ID), Type: string(rec.Type), SubjectKey: rec.SubjectKey, Severity: string(rec.Severity),
		Status: billing.FindingStatus(rec.Status), Evidence: rec.Evidence, LastSeenAt: rec.LastSeenAt, ResolvedAt: rec.ResolvedAt,
		Provider: optionalString(string(rec.Provider)), RecommendedAction: optionalString(rec.RecommendedAction), Resolution: optionalString(rec.Resolution),
		ResolvedBy: optionalString(rec.ResolvedBy), Notes: optionalString(rec.Notes), CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
	if rec.PSPID != nil {
		psp := billing.PSPID(*rec.PSPID)
		v.PSPID = &psp
	}
	if v.Evidence == nil {
		v.Evidence = map[string]any{}
	}
	if parsed, ok := recommend.FromEvidence(rec.Evidence); ok {
		out := recommendationView(parsed)
		v.Recommendation = &out
	}
	if rec.Source != "" {
		source := billing.ProposalSource(rec.Source)
		v.Source = &source
	}
	if rec.Change != nil {
		change := rec.Change.FindingChange
		v.Change = &change
	}
	if rec.PullID != nil {
		pull := billing.PSPImportID(*rec.PullID)
		v.Pull = &pull
	}
	if rec.ApprovalRunID != nil {
		run := billing.RunID(*rec.ApprovalRunID)
		v.ApprovalRunID = &run
	}
	return v
}

func recommendationView(r recommend.Recommendation) billing.FindingRecommendation {
	out := billing.FindingRecommendation{Action: r.Action, Params: r.Params}
	for _, alt := range r.Alternatives {
		out.Alternatives = append(out.Alternatives, recommendationView(alt))
	}
	return out
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
