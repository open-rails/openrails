package billing

const ProductAccessMaxPageSize = 100

type ProductAccessCheckParams struct {
	CustomerID string
	ProductID  string
	ProductKey string
}
type ProductAccessCheckManyParams struct {
	CustomerID  string
	ProductIDs  []string
	ProductKeys []string
}
type ProductAccessListParams struct {
	CustomerID string
	Limit      int
	Cursor     string
}
type ProductAccessList struct {
	Data       []ProductAccessGrant `json:"data"`
	HasMore    bool                 `json:"has_more"`
	NextCursor string               `json:"next_cursor,omitempty"`
}
