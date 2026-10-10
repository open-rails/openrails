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

// Finding is a problem OpenRails found that needs a person, or that it is
// repairing: a discrepancy between OpenRails and a provider (catalog drift
// included), within OpenRails' own books, or in its operation (a provider
// operation refused, a provider event unbooked, stalled background work), and
// what to do about it. A catalog or pull.* finding names the PSP whose read
// raised it; a catalog finding also names the resource, the field and both
// values.
type Finding struct {
	ID                 FindingID      `json:"id"`
	Type               string         `json:"finding_type"`
	Provider           *string        `json:"provider"`
	PSPID              *PSPID         `json:"psp_id"`
	ResourceType       *string        `json:"resource_type"`
	ResourceID         *string        `json:"resource_id"`
	ExternalResourceID *string        `json:"external_resource_id"`
	Field              *string        `json:"field"`
	OpenRailsValue     *string        `json:"openrails_value"`
	ExternalValue      *string        `json:"external_value"`
	SubjectKey         string         `json:"subject_key"`
	Severity           string         `json:"severity"`
	Status             FindingStatus  `json:"status"`
	RecommendedAction  *string        `json:"recommended_action"`
	Evidence           map[string]any `json:"evidence"`
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

// FindingListParams pages the findings queue: most severe first, then
// oldest. Status empty lists the open findings. Type is one finding type or a
// prefix ending in ".*" ("catalog.*" lists catalog drift).
//
// IDs instead reads 1 to MaxBatchItems named findings in one page, open or
// resolved; unknown ones are absent.
type FindingListParams struct {
	PageRequest
	IDs      []FindingID
	Status   FindingStatus
	Severity string
	Type     string
}

// ResolveFindingParams resolves one open finding.
type ResolveFindingParams struct {
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
