module github.com/open-rails/openrails/examples/gated-premium-page

go 1.26.5

// Examples build against the checkout, not a published tag, so breaking
// engine changes update the example in the same PR (tracker #825).
replace github.com/open-rails/openrails => ../..

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/open-rails/authkit v0.89.0
	github.com/open-rails/openrails v0.0.0
)

require github.com/google/uuid v1.6.0 // indirect
