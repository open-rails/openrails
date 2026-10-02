package dbtest

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

// TestMerchantID is the canonical merchant id for fixtures. There is no
// "default merchant" (#336).
var TestMerchantID = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))
