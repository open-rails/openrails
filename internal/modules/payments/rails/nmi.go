// Package rails provides small helpers over the rail (gateway) of a payment.
package rails

import (
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// IsNMI reports whether the rail is the NMI gateway. The rail value IS the
// gateway now (post-#630), so this is a plain equality check — there is no
// longer a name-set mapping multiple rail names onto NMI.
func IsNMI(rail models.Rail) bool {
	return normalize.Lower(string(rail)) == string(models.RailNMI)
}

// SameRail reports whether two rail identifiers name the same rail.
func SameRail(a, b models.Rail) bool {
	left := normalize.Lower(string(a))
	right := normalize.Lower(string(b))
	return left != "" && left == right
}
