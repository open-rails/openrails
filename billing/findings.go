package billing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// FindingID names one finding; on the wire "fnd_<uuid>".
type FindingID uuid.UUID

const FindingIDPrefix = "fnd_"

func ParseFindingID(s string) (FindingID, error) {
	u, err := parsePrefixedID("finding", FindingIDPrefix, s)
	return FindingID(u), err
}

func (id FindingID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id FindingID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id FindingID) String() string               { return formatPrefixedID(FindingIDPrefix, uuid.UUID(id)) }
func (id FindingID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *FindingID) UnmarshalText(b []byte) error {
	v, err := ParseFindingID(string(b))
	*id = v
	return err
}

// FindingStatus is where a finding stands: open (reconcile_required,
// requires_review) or resolved (fixed, admin_fixed, ignored, superseded,
// stale).
type FindingStatus string

// FindingOutcome is how an operator resolves an open finding.
type FindingOutcome string

const (
	// FindingApprove runs the finding's recommendation.
	FindingApprove FindingOutcome = "approve"
	// FindingIgnore silences the finding's subject for good; notes required.
	FindingIgnore FindingOutcome = "ignore"
)

// Finding is one discrepancy reconciliation found between OpenRails and a
// provider, or within OpenRails' own books, and what to do about it.
type Finding struct {
	ID                FindingID      `json:"id"`
	Type              string         `json:"finding_type"`
	Provider          *string        `json:"provider"`
	SubjectKey        string         `json:"subject_key"`
	Severity          string         `json:"severity"`
	Status            FindingStatus  `json:"status"`
	RecommendedAction *string        `json:"recommended_action"`
	Evidence          map[string]any `json:"evidence"`
	// Recommendation is the mechanical fix approving runs; nil when the
	// finding has none and only ignoring resolves it.
	Recommendation *FindingRecommendation `json:"recommendation"`
	LastSeenAt     time.Time              `json:"last_seen_at"`
	ResolvedAt     *time.Time             `json:"resolved_at"`
	Resolution     *string                `json:"resolution"`
	ResolvedBy     *string                `json:"resolved_by"`
	Notes          *string                `json:"operator_notes"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
}

// FindingRecommendation is an action approving a finding runs, with the
// alternatives an operator may pick instead.
type FindingRecommendation struct {
	Action       string                  `json:"action"`
	Params       map[string]any          `json:"params"`
	Alternatives []FindingRecommendation `json:"alternatives"`
}

// ListFindingsRequest pages the findings queue: most severe first, then
// oldest. Status empty lists the open findings.
type ListFindingsRequest struct {
	PageRequest
	Status   FindingStatus
	Severity string
	Type     string
}

// ResolveFindingRequest resolves one open finding.
type ResolveFindingRequest struct {
	Outcome FindingOutcome `json:"outcome"`
	Notes   string         `json:"notes"`
	// OverrideParams replace the recommendation's params when approving.
	OverrideParams json.RawMessage `json:"override_params,omitzero"`
}

// FindingResolution is a resolved finding and what approving it did.
type FindingResolution struct {
	Finding   Finding        `json:"finding"`
	Execution map[string]any `json:"execution"`
}

// FindingSummary is the findings queue at a glance. OrphanedMembers (paying
// without access), Freeloaders (access without paying) and DuplicateCoverage
// (billed twice) should always be zero; VerificationPressure may not be, but
// its age should not grow.
type FindingSummary struct {
	OrphanedMembers      int64                `json:"orphaned_members"`
	Freeloaders          int64                `json:"freeloaders"`
	DuplicateCoverage    int64                `json:"duplicate_coverage"`
	VerificationPressure VerificationPressure `json:"verification_pressure"`
	Episodes             EpisodeTotals        `json:"episodes"`
	OpenBySeverity       map[string]int64     `json:"open_by_severity"`
	TotalOpen            int64                `json:"total_open"`
}

// VerificationPressure counts subscriptions awaiting provider verification
// past their paid-through date, and the oldest one's age.
type VerificationPressure struct {
	Count         int64 `json:"count"`
	MaxAgeSeconds int64 `json:"max_age_seconds"`
}

// EpisodeTotals sums the spans of access without payment (freeloader) and
// payment without access (orphaned).
type EpisodeTotals struct {
	Freeloader FreeloaderEpisodeSummary `json:"freeloader"`
	Orphaned   EpisodeSummary           `json:"orphaned"`
}

// EpisodeSummary is how many spans, how many still open, and their days.
type EpisodeSummary struct {
	Total     int64   `json:"total"`
	Open      int64   `json:"open"`
	TotalDays float64 `json:"total_days"`
}

// FreeloaderEpisodeSummary adds the spans no policy sanctions (dunning and
// awaiting verification are policy).
type FreeloaderEpisodeSummary struct {
	EpisodeSummary
	Unsanctioned int64 `json:"unsanctioned"`
}
