// Package alertjob is the operator alert job and the claim of a finding's
// notification episode that queues it. It sits below every module that raises
// findings; alerting delivers the job.
package alertjob

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// DeliveryKind is the operator alert job.
const DeliveryKind = "openrails.operator_alert"

// ChannelType is a delivery sink kind.
type ChannelType string

const (
	ChannelEmail   ChannelType = "email"
	ChannelWebhook ChannelType = "webhook"
)

// ChannelRef identifies a notification destination. A webhook ref names
// one of the merchant document's alert webhooks; email carries no reference.
type ChannelRef struct {
	Type      ChannelType `json:"type"`
	WebhookID *uuid.UUID  `json:"webhook_id,omitempty"`
}

// DeliveryArgs is one operator alert: its fanout to the configured channels
// (Channel nil), or its delivery to one. Its source is a destructive-action
// hold or a finding's notification episode. It stores channel references,
// never webhook URLs or credentials.
type DeliveryArgs struct {
	MerchantID uuid.UUID     `json:"merchant_id"`
	Hold       *HoldAlert    `json:"hold,omitempty"`
	Finding    *FindingAlert `json:"finding,omitempty"`
	PSP        *PSPAlert     `json:"psp,omitempty"`
	Channel    *ChannelRef   `json:"channel,omitempty"`
}

// HoldAlert is a destructive-action incident.
type HoldAlert struct {
	ID       uuid.UUID `json:"id"`
	Scope    string    `json:"scope"`
	OpenedAt time.Time `json:"opened_at"`
}

// PSPAlert is a PSP's stale episode crossing into warning or critical, or its
// recovery with what the catch-up found: once per crossing.
type PSPAlert struct {
	ID           uuid.UUID  `json:"id"`
	Key          string     `json:"key"`
	Rail         string     `json:"rail"`
	Cause        string     `json:"cause"`
	Severity     string     `json:"severity"`
	StaleSince   time.Time  `json:"stale_since"`
	KnownThrough *time.Time `json:"known_through,omitempty"`
	Recovered    bool       `json:"recovered,omitempty"`
	Renewed      int64      `json:"renewed,omitempty"`
	Failed       int64      `json:"failed,omitempty"`
	Canceled     int64      `json:"canceled,omitempty"`
	Unconfirmed  int64      `json:"unconfirmed,omitempty"`
}

// FindingAlert is one notification episode of a finding: the notified_at its
// claim stamped.
type FindingAlert struct {
	ID         uuid.UUID `json:"id"`
	NotifiedAt time.Time `json:"notified_at"`
}

func (DeliveryArgs) Kind() string { return DeliveryKind }

// InsertOpts retries each delivery on its own. Arguments deduplicate, completed
// jobs included, so a delivered alert is never sent again.
func (DeliveryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 100, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateCompleted, rivertype.JobStatePending,
		rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}

// Valid reports whether a names its merchant and exactly one source.
func (a DeliveryArgs) Valid() bool {
	sources := 0
	for _, set := range []bool{a.Hold != nil, a.Finding != nil, a.PSP != nil} {
		if set {
			sources++
		}
	}
	switch {
	case a.MerchantID == uuid.Nil || sources != 1:
		return false
	case a.Hold != nil:
		return a.Hold.ID != uuid.Nil
	case a.PSP != nil:
		return a.PSP.ID != uuid.Nil
	default:
		return a.Finding.ID != uuid.Nil
	}
}

// Finding is what a finding's notification reads of it.
type Finding struct {
	ID               uuid.UUID
	Status           string
	Severity         string
	Quiet            bool
	NotifiedAt       *time.Time
	NotifiedSeverity string
}

// FindingOf is a stored finding's notification view.
func FindingOf(row gen.BillingReconciliationFinding) Finding {
	var evidence struct {
		Quiet bool `json:"quiet_foreign"`
	}
	_ = json.Unmarshal(row.Evidence, &evidence)
	f := Finding{ID: row.ID, Status: row.Status, Severity: row.Severity, Quiet: evidence.Quiet, NotifiedAt: row.NotifiedAt}
	if row.NotifiedSeverity != nil {
		f.NotifiedSeverity = *row.NotifiedSeverity
	}
	return f
}

// rank orders severities, the gravest first.
func rank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}

// Notify claims f's notification episode at now and queues its alert in one
// transaction of database. Only requires_review findings notify; a set
// NotifiedAt blocks re-firing unless the severity strictly escalates.
func Notify(ctx context.Context, database *db.DB, f Finding, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if f.Status != "requires_review" || f.Quiet {
		return nil
	}
	if f.NotifiedAt != nil && rank(f.Severity) >= rank(f.NotifiedSeverity) {
		return nil // already notified at this severity or worse; not a genuine escalation
	}
	// The episode claim and its alert's job commit together: concurrent or
	// stale callers claim nothing, and a claimed episode is delivered with
	// retries. The episode is the stored instant, so it is microseconds.
	notifiedAt := now.Truncate(time.Microsecond)
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		n, err := database.NewWithPgxTx(tx).Gen(ctx).ClaimReconciliationFindingNotification(ctx, gen.ClaimReconciliationFindingNotificationParams{
			MerchantID: mid.UUID(), ID: f.ID, NotifiedAt: notifiedAt, Severity: f.Severity,
		})
		if err != nil || n == 0 {
			return err
		}
		return queue(ctx, database, tx, mid.UUID(), f.ID, f.Severity, notifiedAt)
	})
	if err != nil {
		return fmt.Errorf("claim finding notification: %w", err)
	}
	return nil
}

// queue commits the alert of a finding's notification episode in tx, the
// transaction that claimed it. A low finding stays in the console.
func queue(ctx context.Context, database *db.DB, tx pgx.Tx, merchantID, findingID uuid.UUID, severity string, notifiedAt time.Time) error {
	if severity == "low" {
		return nil
	}
	_, err := database.InsertRiverJobTx(ctx, tx, DeliveryArgs{MerchantID: merchantID, Finding: &FindingAlert{ID: findingID, NotifiedAt: notifiedAt}}, nil)
	if err != nil {
		return fmt.Errorf("queue finding alert: %w", err)
	}
	return nil
}
