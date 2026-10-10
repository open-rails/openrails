// Package openrails is the OpenRails Go SDK: one Client with two constructors.
// New(ctx, cfg, deps) runs the engine in the host process, applying its
// migrations first, and serves calls through the same handlers the HTTP server
// mounts; it adds Start, Close, Routes, RiverJobs, Ready and Probes.
// NewRemote(baseURL, opts...) talks to a standalone OpenRails over HTTP.
// Request, response and identifier types, errors and codes live in package billing.
package openrails
