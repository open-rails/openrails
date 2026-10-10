package billing

// AskCatalogParams is a question about the merchant's catalog.
type AskCatalogParams struct {
	Question string `json:"question"`
}

// CatalogAnswer is a model's answer to a catalog question, with every lookup
// it ran and the changes it proposes. A draft changes nothing: a person
// reviews it and sends its requests.
type CatalogAnswer struct {
	Answer   string            `json:"answer"`
	Evidence []CatalogEvidence `json:"evidence"`
	Drafts   []CatalogDraft    `json:"drafts"`
}

// CatalogEvidence is one lookup a catalog answer ran: the tool, its arguments
// and the text the model read.
type CatalogEvidence struct {
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	Summary string `json:"summary"`
}

// CatalogDraftKind names the one field a CatalogDraft sets.
type CatalogDraftKind string

const (
	CatalogDraftPriceChange CatalogDraftKind = "price_change"
	CatalogDraftNewPrice    CatalogDraftKind = "new_price"
	CatalogDraftRefused     CatalogDraftKind = "refused"
)

// CatalogDraft is one change a catalog answer proposes, or the reason it
// proposes none.
type CatalogDraft struct {
	Kind        CatalogDraftKind     `json:"kind"`
	PriceChange *PriceChangeDraft    `json:"price_change"`
	NewPrice    *NewPriceDraft       `json:"new_price"`
	Refusal     *CatalogDraftRefusal `json:"refusal"`
}

// PriceChangeDraft proposes a new version of the price under PriceKey.
// CreatePrice creates it; Migration, when set, moves the existing subscribers
// to it (its to_price_id is the created price), and they keep their price
// when it is nil.
type PriceChangeDraft struct {
	ProductKey    string `json:"product_key"`
	PriceKey      string `json:"price_key"`
	CurrentAmount int64  `json:"current_amount,string"`
	NewAmount     int64  `json:"new_amount,string"`
	Currency      string `json:"currency"`
	// AffectedCount is how many subscribers hold an earlier version.
	AffectedCount int                         `json:"affected_count"`
	ReviewText    string                      `json:"review_text"`
	CreatePrice   CreatePriceParams           `json:"create_price"`
	Migration     *CreatePriceMigrationParams `json:"migration"`
}

// NewPriceDraft proposes another price on an existing product.
type NewPriceDraft struct {
	ProductKey  string            `json:"product_key"`
	ReviewText  string            `json:"review_text"`
	CreatePrice CreatePriceParams `json:"create_price"`
}

// CatalogDraftRefusal is why a requested change was not drafted, and what to
// do instead. Code is cross_product, cross_currency, inactive_price,
// same_product_migration or customer_amount_price.
type CatalogDraftRefusal struct {
	Code       string `json:"code"`
	Reason     string `json:"reason"`
	Workaround string `json:"workaround"`
}
