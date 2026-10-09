package authtest

import "github.com/open-rails/openrails/internal/config"

// Perm is a host's permission, as a guard names it.
type Perm string

func (p Perm) String() string { return string(p) }

// The permissions Guards names.
const (
	StaffRead  = "staff:read"
	StaffWrite = "staff:write"
	StaffAdmin = "staff:admin"
)

// Guards guards each staff level group with a permission naming it:
// openrails' StaffReads, StaffWrites and MerchantConfig.
func Guards() config.Guards {
	return config.Guards{"staff_reads": Perm(StaffRead), "staff_writes": Perm(StaffWrite), "merchant_config": Perm(StaffAdmin)}
}

// StaffGuards is Guards without MerchantConfig's: a mount of Routes.Merchant
// alone.
func StaffGuards() config.Guards {
	return config.Guards{"staff_reads": Perm(StaffRead), "staff_writes": Perm(StaffWrite)}
}
