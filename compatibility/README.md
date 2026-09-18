# Reviewed release contract

`contract.json` is derived from source by `internal/contractaudit`; there is no
handwritten catalog. It records:

- `go_api`: exported declarations of every importable non-main package (root,
  `config`, `embed`, `permissions`, `migrations/postgres`, `pkg/...`), including
  generic signatures, receivers, aliases, constant values and JSON tags. Internal
  types exposed through public aliases, fields or signatures are followed
  transitively, including their exported methods; unrelated internal types and
  implementation bodies are not frozen.
- `wire_types`: JSON-tagged struct shapes in internal and command packages, and
  full custom JSON/text codec methods in any package.
- `declaring_file_imports`: import paths behind the selectors above.
- `boundary_sources_sha256`: comment-insensitive fingerprints of route,
  authority, status/error-mapping implementation (`internal/http`,
  `internal/auth`, `internal/controlplane`, `internal/requestauth`,
  `pkg/billingauth`, `pkg/api`, `permissions`, root `errors.go`, and every
  other production file importing `permissions`, `pkg/billingauth` or
  `internal/auth/policy`), the fresh SQL baseline and `testdata/wire` fixtures.

`TestReviewedReleaseContract` runs in the unit gate. After reviewing an
intentional pre-v1 change, run `go run ./scripts/contracts -write` and commit the
snapshot. `TestContractGateDetectsCoveredMutations` proves each category by
mutating real files in a copy-on-write view of the repository.

Limits: fingerprints are review tripwires, not a semantic compatibility
analyzer; anonymous response structs and maps are covered only inside boundary
fingerprints. Behavior needs the workflow matrix below.

## Workflow matrix

`workflows.tsv` is the release matrix: six scenarios × embedded, standalone and
SaaS. A retained row names a real integration test path (a subtest names the
deployment); `gap` and `pending#<PR>` rows state what is missing. The unit gate
rejects incomplete matrices and retained rows that do not name an
integration-tagged test.

`go run ./scripts/contracts -workflows` runs the retained rows through
`scripts/test_integration.sh` and writes `go test -json` events to `.reports/`.
Only explicit passes qualify: skips (including skipped subtests), failures,
build failures, missing tests and any gap or pending cell fail. The browser
sender-proof job, openrails-saas suites and provider qualification remain
separate required evidence; this manifest does not imply live PSP behavior.

No v1 tag is created by this tool. Before v1, regenerate after all planned
reductions and enforce compatibility against the published tag.
