# Reviewed release contract

`contract.json` records exported Go declarations, including generic signatures,
receiver types, aliases and JSON tags. It includes all currently public Go
packages so a helper accidentally used by a consumer cannot disappear silently.
It also hashes the fresh SQL baseline, canonical wire fixtures, route registry,
authorization boundary and HTTP handlers (including anonymous response maps).

The unit gate runs on every source change. To update the pre-v1 candidate after
reviewing an intentional hard cut, run `go run ./scripts/contracts -write` from
the repository root and commit the snapshot with the change. This does not
freeze a v1 release or prove behavioral compatibility: the real deployment
workflow suites and provider qualification gates remain mandatory.

Before declaring v1, regenerate after all planned reductions, pin the supported
release tag, and enforce migration immutability/API compatibility against that
tag. No v1 tag is created by this tool, and updating this pre-v1 snapshot is not
permission to break a published v1 contract.

`python3 scripts/check-v1-workflows.py` runs the small release workflow manifest
against disposable PostgreSQL/Redis fixtures and requires every named test to
actually pass. Missing/renamed tests and skips fail instead of producing an
empty green suite. It writes JSON test evidence under `.reports/`. The browser
sender-proof/cookie workflow remains a separate required CI job. The actual SaaS
consumer repository must also pass its fee, identity and hosted-routing suites;
the core harness alone is not proof of that integration.
