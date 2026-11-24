#!/usr/bin/env bash
# The library borrows every connection through Deps: only the server binary's
# hostdeps, the internal/db session pools and tooling open one;
# adapters/smtpmail only wraps a sender the host opened.
set -euo pipefail
cd "$(dirname "$0")/.."
hits="$(git grep -n -E 'pgxpool\.New(WithConfig)?\(|redis\.New(Client|UniversalClient|ClusterClient|FailoverClient|SentinelClient)\(|vaultapi\.NewClient\(|"github.com/open-rails/helpers/vault"|"github.com/open-rails/helpers/smtp"' -- '*.go' \
  ':!*_test.go' ':!server/' ':!examples/' ':!scripts/' \
  ':!adapters/smtpmail/' ':!internal/db/session.go' ':!internal/db/commit_guard.go' ':!internal/schemasnapshot/' ':!internal/vaulttest/' || true)"
if [[ -n "$hits" ]]; then
  printf 'The library opens no connection; the host passes it in Deps:\n%s\n' "$hits" >&2
  exit 1
fi
