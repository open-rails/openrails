// Package openrails is the OpenRails Go SDK: one Client with two constructors.
//
//   - New(ctx, cfg, deps) runs the engine in the host process. The Client
//     talks to it over an in-process transport into the same handlers the HTTP
//     server mounts, and adds the hosting operations: Start, Close, Routes,
//     RiverJobs, Ready and Probes.
//   - NewRemote(baseURL, opts...) talks to a standalone OpenRails over HTTP.
//
// Migrate creates OpenRails' tables before New. Request, response and
// identifier types, errors and codes live in package billing; Config and
// Deps are defined in internal/config and named here.
package openrails
