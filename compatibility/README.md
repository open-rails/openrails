# Compatibility contracts

What v1 freezes and how a change is reviewed is in
[docs/compatibility.md](../docs/compatibility.md). The three snapshots are in
`api/`: `go.txt` (the Go API), `openapi.json` (the HTTP API) and `schema.txt`
(the database schema).

`contract.json` here is a fourth, finer guard: the source of every exported
declaration of the public packages, the internal types they reach, and the JSON
shapes and custom codecs of every wire type. A change to any of them shows in
its diff even when the public signature is unchanged.

Regenerate all of them with `go run ./scripts/contracts -write` (with
`OPENRAILS_E2E_DSN` naming a disposable PostgreSQL 18 for the schema) and review
the diff; `go run ./scripts/contracts` checks them, as the tests do.
