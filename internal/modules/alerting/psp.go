package alerting

import (
	"fmt"
	"time"
)

// pspAlert tells the merchant what is known of a PSP, what is not, since
// when, why, and the fix; or that it was caught up and what the gap hid.
func (s *Service) pspAlert(p PSPAlert) Alert {
	name := fmt.Sprintf("%s (%s)", p.Key, p.Rail)
	through := "nothing yet"
	if p.KnownThrough != nil {
		through = p.KnownThrough.UTC().Format(time.RFC3339)
	}
	link := s.opsLink("psp=" + p.ID.String())
	if p.Recovered {
		return Alert{Title: "PSP data is up to date: " + name, Severity: SeverityWarning, DashboardLink: link, FiredAt: p.StaleSince,
			Summary: fmt.Sprintf("OpenRails reads %s again and has read the gap since %s: during it %d renewals were paid, %d failed, %d subscriptions were canceled, and %d are still unconfirmed.",
				name, through, p.Renewed, p.Failed, p.Canceled, p.Unconfirmed)}
	}
	severity := SeverityWarning
	if p.Severity == "critical" {
		severity = SeverityCritical
	}
	return Alert{Title: "PSP data is out of date: " + name, Severity: severity, DashboardLink: link, FiredAt: p.StaleSince,
		Summary: fmt.Sprintf("OpenRails' record of %s is complete through %s and stale since %s: %s Its subscriptions keep their last known state and access; nothing is inferred from the gap, and it is read as soon as it can be.",
			name, through, p.StaleSince.UTC().Format(time.RFC3339), pspFix(p.Cause))}
}

// pspFix is a stale cause in words, with what fixes it.
func pspFix(cause string) string {
	switch cause {
	case "credentials_refused":
		return "the provider refuses its credentials. Update them."
	case "not_configured":
		return "it has no credentials to read with. Add them."
	case "imported":
		return "its data came from an import and it was never connected. Connect it to bring the data up to date."
	case "unreachable":
		return "the provider is not answering. Catch-up is automatic once it does."
	}
	return "its gap is being read."
}
