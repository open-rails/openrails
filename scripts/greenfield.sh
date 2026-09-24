#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
: "${OPENRAILS_GREENFIELD_DSN:?Set OPENRAILS_GREENFIELD_DSN to a disposable PostgreSQL database}"

flags=(-vet=all -race -count=1 -tags='greenfield,integration' -timeout "${OPENRAILS_GREENFIELD_TIMEOUT:-8m}")

# Build once, then run the multi-replica fleets (#1075) in their own process
# beside the other contracts; both share the database.
go test "${flags[@]}" -run '^$' ./ci/greenfield/...
(
	set -o pipefail
	go test "${flags[@]}" -p 1 -parallel 3 -run '^TestReplicas' ./ci/greenfield/subscriptions 2>&1 | sed -u 's/^/[replicas] /'
) &
replicas=$!
# The adversarial security cases (#1077, docs/security-tests.md) likewise.
(
	set -o pipefail
	go test "${flags[@]}" -p 1 -parallel 3 -run '^TestSecurity' ./ci/greenfield/... 2>&1 | sed -u 's/^/[security] /'
) &
security=$!
status=0
go test "${flags[@]}" -p 1 -parallel 4 -skip '^(TestReplicas|TestSecurity)' ./ci/greenfield/... || status=$?
wait "$replicas" || status=$?
wait "$security" || status=$?
exit "$status"
