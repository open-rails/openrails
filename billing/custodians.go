package billing

// CardVaultKind is a card vault's vendor.
type CardVaultKind string

const (
	CardVaultBasisTheory CardVaultKind = "basis_theory"
	CardVaultHyperSwitch CardVaultKind = "hyperswitch"
)

// Custodian is the merchant's declared card-vault account. Credentials report
// presence only. The hosted platform vault is selected through CardVault and
// is not a merchant-owned custodian declaration.
type Custodian struct {
	Key         string                   `json:"key"`
	Kind        CardVaultKind            `json:"kind"`
	Environment Environment              `json:"environment"`
	AccountID   string                   `json:"account_id"`
	Settings    map[string]any           `json:"settings"`
	Credentials map[string]PSPCredential `json:"credentials"`
	Archived    bool                     `json:"archived"`
	Revision    int64                    `json:"revision"`
}

// PutCustodianParams creates or replaces one merchant-owned vault document.
// ExpectedRevision is zero for a new key; edits name the revision read. Kind,
// environment and account identity never change. Omitted settings/credentials
// retain their values; supplied credential values are written, never returned.
type PutCustodianParams struct {
	ExpectedRevision int64             `json:"expected_revision"`
	Kind             CardVaultKind     `json:"kind"`
	Environment      Environment       `json:"environment"`
	AccountID        string            `json:"account_id"`
	Settings         map[string]any    `json:"settings,omitempty"`
	Credentials      map[string]string `json:"credentials,omitempty"`
}
