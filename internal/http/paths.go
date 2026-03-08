package server

// StandaloneV1Prefix is the canonical API prefix for standalone mode.
const StandaloneV1Prefix = "/v1"

// EmbeddedV1Prefix is the canonical API prefix for embedded mode handlers.
//
// Embedded hosts typically mount billing under "/billing", so the stable contract becomes
// "/billing/v1/*".
const EmbeddedV1Prefix = "/billing/v1"
