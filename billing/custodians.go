package billing

// Custodian is the merchant's declared card-vault account. Credentials report
// presence only. The hosted platform vault is selected through CardVault and
// is not a merchant-owned custodian declaration.
type Custodian struct {
	Key         string                   `json:"key"`
	Kind        string                   `json:"kind"`
	Environment string                   `json:"environment"`
	AccountID   string                   `json:"account_id"`
	Settings    map[string]any           `json:"settings"`
	Credentials map[string]PSPCredential `json:"credentials"`
	Archived    bool                     `json:"archived"`
	Revision    int64                    `json:"revision"`
}
