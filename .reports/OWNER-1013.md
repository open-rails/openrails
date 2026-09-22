Owner: /root/astra_retry_compare
Purpose: #1013 CI package partition with preserved race/default-only coverage
Branch: ci/1013-package-partition-20260922
Base: fetched origin/master ca38c10d4e1a10c89cde6a0a71b2677e6706f291
Scope: scripts/check.sh, scripts/test_integration.sh and existing wrapper fixtures. No money/worker source, test deletion, manifest reduction or #1032 overlap.
Checks retains source/security/build/vet/SQL gates; E2E retains its integration package set and gains race. Whole-package fallback preserves default-only source/test files.
Authorized extension after E2E exposed a production race: isolate the LIFE pass lifecycle clock in a local value copy; two production files only, no test/lock/serialization workaround. Root approved design and exact diff after negative/fixed evidence.
