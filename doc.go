// Package openrails is the OpenRails Go SDK. A Client is a handle on one
// merchant, with every merchant-scoped operation, and comes from one of two
// places. New(ctx, cfg, deps) runs the Engine in the host process, applying
// its migrations first: the Engine has Start, Close, Routes, RiverJobs, Ready
// and Probes, and Engine.Merchant(id) is a merchant's handle, served by the
// same handlers the HTTP server mounts. NewRemote(baseURL, opts) is a
// standalone OpenRails over HTTP, holding a credential per merchant:
// Remote.Merchant(id) is the same handle, and NewRemoteMerchant one
// merchant's directly.
// Request, response and identifier types, errors and codes live in package billing.
package openrails
