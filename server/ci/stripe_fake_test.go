//go:build e2e && integration

package ci_test

import "github.com/open-rails/openrails/internal/stripemock"

var visa = stripemock.Card{Brand: "visa", Last4: "4242"}

const (
	whsecStripe = "whsec_e2e"
	monthHours  = 720
)
