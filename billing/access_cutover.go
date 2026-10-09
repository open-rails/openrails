package billing

// AccessCutoverReport is what the cutover from per-key entitlement windows to
// product access does: each key it changes for a customer from then on, and
// the windows it converts with a note.
type AccessCutoverReport struct {
	Changes []AccessChange `json:"changes"`
	Notes   []AccessNote   `json:"notes"`
}

// Unapproved counts the changes the cutover refuses until they are approved.
func (r AccessCutoverReport) Unapproved() int {
	n := 0
	for _, c := range r.Changes {
		if !c.Approved {
			n++
		}
	}
	return n
}

// AccessChange is one key whose access the cutover changes for a customer.
type AccessChange struct {
	MerchantID  MerchantID       `json:"merchant_id"`
	CustomerID  CustomerID       `json:"customer_id"`
	Entitlement string           `json:"entitlement"`
	Change      AccessChangeKind `json:"change"`
	Approved    bool             `json:"approved"`
}

// AccessChangeKind is whether a customer loses or gains a key at the cutover.
type AccessChangeKind string

const (
	// AccessChangeLost: the customer's product dropped the key after they got
	// it.
	AccessChangeLost AccessChangeKind = "lost"
	// AccessChangeGained: the customer's product added the key since.
	AccessChangeGained AccessChangeKind = "gained"
)

// AccessNote is a converted window besides its key changes.
type AccessNote struct {
	MerchantID   MerchantID     `json:"merchant_id"`
	CustomerID   CustomerID     `json:"customer_id"`
	Note         AccessNoteKind `json:"note"`
	SourceType   string         `json:"source_type"`
	SourceID     string         `json:"source_id"`
	Entitlements []string       `json:"entitlements"`
}

// AccessNoteKind is what an AccessNote says.
type AccessNoteKind string

const (
	// AccessNoteUnmapped: a live window no product grants (e.g. a manual key
	// grant); the cutover cannot carry it.
	AccessNoteUnmapped AccessNoteKind = "unmapped"
	// AccessNoteMixedDuration: a purchase whose keys had different durations;
	// it keeps the longest.
	AccessNoteMixedDuration AccessNoteKind = "mixed_duration"
)
