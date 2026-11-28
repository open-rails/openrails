package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// PSPFreshness is how far OpenRails' record of one PSP is complete: its
// known-through, and why the record stops there. A renewal due after a PSP's
// known-through is not overdue, and nothing is inferred from its silence.
type PSPFreshness struct {
	// KnownThrough is the instant through which the record is complete: now
	// while the PSP is current, else its latest evidence (a read, a webhook
	// or an import); null when there is none.
	KnownThrough *time.Time `json:"known_through"`
	// Source is the evidence KnownThrough comes from.
	Source PSPKnownThroughSource `json:"source"`
	// Current is a PSP read successfully and caught up: its record is complete
	// through now.
	Current bool `json:"current"`
	// StaleSince is when the record stopped being complete; null while current.
	StaleSince *time.Time `json:"stale_since"`
	// Cause is why the PSP is stale, or what a current one needs (silent
	// webhooks); empty when nothing does.
	Cause    PSPStaleCause    `json:"cause"`
	Severity PSPStaleSeverity `json:"severity"`
	// LastReadAt is the last provider read that succeeded; FailingSince the
	// first failed read since.
	LastReadAt   *time.Time `json:"last_read_at"`
	FailingSince *time.Time `json:"failing_since"`
	// CatchUp is what the last catch-up read found in its gap; null before one.
	CatchUp *PSPCatchUp `json:"catch_up"`
	// HasData is a PSP holding subscriptions or payments.
	HasData bool `json:"has_data"`
	// Awaiting is what waits on the provider, per currency: subscriptions
	// unverified awaiting it, and a stale PSP's active ones whose renewal
	// came due. Counted beside at-risk, never in it.
	Awaiting []PSPAwaiting `json:"awaiting"`
}

// PSPAwaiting is the subscriptions and their MRR in one currency whose state
// awaits their provider.
type PSPAwaiting struct {
	Currency      catalog.Currency `json:"currency"`
	Subscriptions int64            `json:"subscriptions"`
	MRR           int64            `json:"mrr,string"`
}

// PSPKnownThrough is one PSP's known-through, as every metrics read reports
// it.
type PSPKnownThrough struct {
	PSPID PSPID  `json:"psp_id"`
	Key   string `json:"key"`
	Rail  Rail   `json:"rail"`
	PSPFreshness
}

// PSPKnownThroughSource is the evidence a PSP's known-through comes from.
type PSPKnownThroughSource string

const (
	// KnownThroughNow: the PSP is current.
	KnownThroughNow PSPKnownThroughSource = "now"
	// KnownThroughRead: its last successful provider read.
	KnownThroughRead PSPKnownThroughSource = "read"
	// KnownThroughWebhook: its last verified webhook.
	KnownThroughWebhook PSPKnownThroughSource = "webhook"
	// KnownThroughImport: the evidence instant of what an import brought.
	KnownThroughImport PSPKnownThroughSource = "import"
	// KnownThroughNone: nothing was ever read or imported of it.
	KnownThroughNone PSPKnownThroughSource = "none"
)

// PSPStaleCause is why a PSP's record is not complete through now.
type PSPStaleCause string

const (
	// PSPImported: an import brought its data and it was never connected.
	PSPImported PSPStaleCause = "imported"
	// PSPNotConfigured: it has no credentials to read with.
	PSPNotConfigured PSPStaleCause = "not_configured"
	// PSPCredentialsRefused: the provider refuses its credentials.
	PSPCredentialsRefused PSPStaleCause = "credentials_refused"
	// PSPUnreachable: the provider does not answer; catch-up is automatic.
	PSPUnreachable PSPStaleCause = "unreachable"
	// PSPCatchingUp: readable again, its gap is being read.
	PSPCatchingUp PSPStaleCause = "catching_up"
	// PSPWebhooksSilent: a current PSP whose webhooks went silent while its
	// reads succeed; its endpoint needs registering again.
	PSPWebhooksSilent PSPStaleCause = "webhooks_silent"
)

// PSPStaleSeverity is how much a PSP's cause matters: info while an import is
// onboarded or a read gap is inside its rail's renewal deadline, warning past
// it with live subscriptions, critical when the provider refuses the
// credentials.
type PSPStaleSeverity string

const (
	PSPSeverityInfo     PSPStaleSeverity = "info"
	PSPSeverityWarning  PSPStaleSeverity = "warning"
	PSPSeverityCritical PSPStaleSeverity = "critical"
)

// PSPCatchUp is what a catch-up read found in the gap From..Through: the
// renewals paid and failed in it, the subscriptions canceled in it, and those
// a read left unconfirmed.
type PSPCatchUp struct {
	From        time.Time `json:"from"`
	Through     time.Time `json:"through"`
	Renewed     int64     `json:"renewed"`
	Failed      int64     `json:"failed"`
	Canceled    int64     `json:"canceled"`
	Unconfirmed int64     `json:"unconfirmed"`
}

// PSPHostEvent is psp.stale (a PSP's stale episode reached warning or
// critical) or psp.recovered (that episode ended, with its catch-up): once
// per crossing.
type PSPHostEvent struct {
	PSPID        PSPID            `json:"psp_id"`
	Key          string           `json:"key"`
	Rail         Rail             `json:"rail"`
	Cause        PSPStaleCause    `json:"cause"`
	Severity     PSPStaleSeverity `json:"severity"`
	StaleSince   time.Time        `json:"stale_since"`
	KnownThrough *time.Time       `json:"known_through"`
	CatchUp      *PSPCatchUp      `json:"catch_up"`
}

// UnverifiedReason is why an unverified subscription is.
type UnverifiedReason string

const (
	// UnverifiedAwaitingProvider: its provider was not read since it became
	// unverified; our record is stale. Not at risk.
	UnverifiedAwaitingProvider UnverifiedReason = "awaiting_provider"
	// UnverifiedUnconfirmed: its provider was read and could not tell.
	UnverifiedUnconfirmed UnverifiedReason = "unconfirmed"
)
