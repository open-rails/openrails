package catalog

import (
	"bytes"
	"encoding/json"
)

// CreditGrantSpec describes the prepaid currency balance delivered by a
// successful payment. Exactly one of Amount and FromPayment is required.
// The accepted terms are frozen at checkout; later catalog edits do not
// alter a purchased lot. Expiry starts when the credits become available.
type CreditGrantSpec struct {
	Currency         string `json:"currency"`
	Amount           *int64 `json:"amount,omitempty,string"`
	FromPayment      bool   `json:"from_payment,omitempty"`
	ExpiresAfterDays *int   `json:"expires_after_days,omitempty"`
}

// CustomerAmount is an immutable, inclusive range, in native units of the
// price's currency, that a customer may choose for a one-off credit deposit.
// The price's UnitAmount is zero; checkout requires an amount within it.
type CustomerAmount struct {
	MinAmount int64 `json:"min_amount,string"`
	MaxAmount int64 `json:"max_amount,string"`
}

// UnmarshalJSON accepts exact numeric YAML values as well as the decimal
// strings used on the JSON wire. Field[int64] never passes through float64.
func (c *CreditGrantSpec) UnmarshalJSON(raw []byte) error {
	var value struct {
		Currency         string       `json:"currency"`
		Amount           Field[int64] `json:"amount"`
		FromPayment      bool         `json:"from_payment"`
		ExpiresAfterDays *int         `json:"expires_after_days"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*c = CreditGrantSpec{Currency: value.Currency, FromPayment: value.FromPayment, ExpiresAfterDays: value.ExpiresAfterDays}
	if value.Amount.Set && !value.Amount.Null {
		c.Amount = &value.Amount.Value
	}
	return nil
}

func (c *CustomerAmount) UnmarshalJSON(raw []byte) error {
	var value struct {
		MinAmount Field[int64] `json:"min_amount"`
		MaxAmount Field[int64] `json:"max_amount"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*c = CustomerAmount{MinAmount: value.MinAmount.Value, MaxAmount: value.MaxAmount.Value}
	return nil
}
