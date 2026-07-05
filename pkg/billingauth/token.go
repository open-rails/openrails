package billingauth

// TokenAudience is the fixed `aud` claim OpenRails' control plane issues and
// expects on its own tokens (issued access tokens, delegated self-service
// tokens, the JWKS-external verifier registration, ...). It is an OpenRails
// PRODUCT constant, not a per-host/per-deployment config value — every
// internal mint/verify site and every embedding host must use this exact
// constant so the two can never drift apart (#750).
const TokenAudience = "openrails"
