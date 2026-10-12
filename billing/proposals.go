package billing

import (
	"time"

	"github.com/google/uuid"
)

// RunID names one maintenance run, such as the approval run that applied a
// call's proposals; on the wire "run_<uuid>".
type RunID uuid.UUID

const RunIDPrefix = "run_"

func ParseRunID(s string) (RunID, error) {
	u, err := parsePrefixedID("run", RunIDPrefix, s)
	return RunID(u), err
}

func (id RunID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id RunID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id RunID) String() string               { return formatPrefixedID(RunIDPrefix, uuid.UUID(id)) }
func (id RunID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *RunID) UnmarshalText(b []byte) error {
	v, err := ParseRunID(string(b))
	*id = v
	return err
}

// Proposal refusal codes.
const (
	// CodeFindingRequiresMerchantConfig: ResolveFinding refuses a pull.*
	// finding; it is a proposal.
	CodeFindingRequiresMerchantConfig = "finding_requires_merchant_config"
	// CodeDestructiveActionsHeld: the kill switch holds the proposal.
	CodeDestructiveActionsHeld = "destructive_actions_held"
	// CodeProviderWritesHeld: a readonly write posture holds the proposal.
	CodeProviderWritesHeld = "provider_writes_held"
)

// ChangeKind is what approving a proposal does. The set is closed.
type ChangeKind string

const (
	// ChangeCreateSubscription creates a subscription the provider bills and
	// OpenRails does not know, with its access and latest charge.
	ChangeCreateSubscription ChangeKind = "create_subscription"
	// ChangeRecordCharge records a provider charge missing locally.
	ChangeRecordCharge ChangeKind = "record_charge"
	// ChangeRecordRefund records a provider refund missing locally.
	ChangeRecordRefund ChangeKind = "record_refund"
	// ChangeAdoptSubscription renews, adopts the period end, sets past due or
	// parks the subscription unverified, through the lifecycle decider.
	ChangeAdoptSubscription ChangeKind = "adopt_subscription"
	// ChangeCancelSubscription cancels the subscription and ends its access,
	// with no provider write.
	ChangeCancelSubscription ChangeKind = "cancel_subscription"
	// ChangeUpdateCard takes the provider's card as a card version.
	ChangeUpdateCard ChangeKind = "update_card"
	// ChangeRevokeAccess revokes the access a reversed or disputed charge
	// paid for.
	ChangeRevokeAccess ChangeKind = "revoke_access"
	// ChangeRefundChargeAfterCancel queues the full refund of a charge that
	// landed after its subscription was canceled.
	ChangeRefundChargeAfterCancel ChangeKind = "refund_charge_after_cancel"
	// ChangeCancelProviderSchedule queues the provider cancel of an
	// OpenRails-owned schedule whose subscription ended.
	ChangeCancelProviderSchedule ChangeKind = "cancel_provider_schedule"
	// ChangeCollectSkippedRenewal queues the collection of a renewal the
	// provider skipped.
	ChangeCollectSkippedRenewal ChangeKind = "collect_skipped_renewal"
)

// ChangeKinds is every change kind.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{ChangeCreateSubscription, ChangeRecordCharge, ChangeRecordRefund, ChangeAdoptSubscription,
		ChangeCancelSubscription, ChangeUpdateCard, ChangeRevokeAccess, ChangeRefundChargeAfterCancel,
		ChangeCancelProviderSchedule, ChangeCollectSkippedRenewal}
}

// ProposalSource is what raised a proposal: a PSP read, or a by-id read of a
// record OpenRails was uncertain of.
type ProposalSource string

const (
	ProposalSourcePull         ProposalSource = "pull"
	ProposalSourceVerification ProposalSource = "verification"
)

// FindingChange is the change a pull.* finding proposes: what approving it
// does, to which subject, from what to what, with which effects, against
// which basis and on what provider evidence.
type FindingChange struct {
	Kind       ChangeKind     `json:"kind"`
	Subject    ChangeSubject  `json:"subject"`
	CustomerID *CustomerID    `json:"customer_id"`
	From       map[string]any `json:"from"`
	To         map[string]any `json:"to"`
	Effects    ChangeEffects  `json:"effects"`
	// Basis is the state the change was computed against; approving it when
	// the basis no longer holds supersedes it instead.
	Basis      ChangeBasis    `json:"basis"`
	ObservedAt time.Time      `json:"observed_at"`
	Evidence   ChangeEvidence `json:"evidence"`
}

// ChangeSubject names the record a change acts on: a subscription, payment,
// payment method or transaction, by its wire id (a transaction by its
// provider id).
type ChangeSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ChangeEffects are what holds a change: the kill switch holds EndsAccess and
// ProviderWrite, a readonly write posture MovesMoney and ProviderWrite.
type ChangeEffects struct {
	EndsAccess    bool `json:"ends_access"`
	MovesMoney    bool `json:"moves_money"`
	ProviderWrite bool `json:"provider_write"`
}

// ChangeBasis is one of: a subscription's row version, the provider id that
// must still be unrecorded, or a payment method's current card version.
type ChangeBasis struct {
	RowVersion    *int64  `json:"row_version"`
	AbsentRailID  *string `json:"absent_rail_id"`
	CardVersionID *string `json:"card_version_id"`
}

// ChangeEvidence is the provider's record the change rests on, and how it was
// read (roster, window or by_id).
type ChangeEvidence struct {
	Read     string         `json:"read"`
	Provider map[string]any `json:"provider"`
}

// ProposalListParams pages proposals: open pull.* findings unless Status is
// set, most severe first, then oldest. IDs instead reads 1 to MaxBatchItems
// named proposals in one page.
type ProposalListParams struct {
	PageRequest
	IDs    []FindingID
	PSPID  PSPID
	Kind   ChangeKind
	Source ProposalSource
	Status FindingStatus
}

// ApproveProposalsParams approves 1 to MaxBatchItems proposals, each applied
// in its own transaction.
type ApproveProposalsParams struct {
	IDs   []FindingID `json:"ids"`
	Notes string      `json:"notes"`
}

// DismissProposalsParams dismisses 1 to MaxBatchItems proposals; notes are
// required.
type DismissProposalsParams struct {
	IDs   []FindingID `json:"ids"`
	Notes string      `json:"notes"`
}

// ProposalOutcome is what one approve or dismiss did to one proposal.
type ProposalOutcome string

const (
	// ProposalApproved: the change applied.
	ProposalApproved ProposalOutcome = "approved"
	// ProposalDismissed: closed without applying.
	ProposalDismissed ProposalOutcome = "dismissed"
	// ProposalAlreadyResolved: resolved earlier; its resolver and time are on
	// the finding.
	ProposalAlreadyResolved ProposalOutcome = "already_resolved"
	// ProposalSuperseded: the basis moved or the evidence went stale; the
	// finding is resolved so and nothing applied.
	ProposalSuperseded ProposalOutcome = "superseded"
	// ProposalHeld: the kill switch or a readonly posture holds it; it stays
	// open.
	ProposalHeld ProposalOutcome = "held"
	// ProposalFailed: applying failed; it stays open with the error in its
	// notes.
	ProposalFailed ProposalOutcome = "failed"
)

// ProposalResults is one approve or dismiss call: one result per id, in
// request order. RunID is the call's approval run, nil when nothing applied.
type ProposalResults struct {
	RunID *RunID           `json:"run_id"`
	Data  []ProposalResult `json:"data"`
}

// ProposalResult is one id's outcome.
type ProposalResult struct {
	ID        FindingID       `json:"id"`
	Outcome   ProposalOutcome `json:"outcome"`
	Finding   *Finding        `json:"finding"`
	Execution map[string]any  `json:"execution"`
	Error     *ProposalError  `json:"error"`
}

// ProposalError is why a proposal was held or failed: a registered code and a
// message.
type ProposalError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
