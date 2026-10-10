package billingauth

// TokenAudience is the fixed `aud` claim of OpenRails' own tokens. It is a
// product constant, not configuration: every mint and verify site and every
// embedding host uses it, so they cannot drift.
const TokenAudience = "openrails"
