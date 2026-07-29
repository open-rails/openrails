#!/usr/bin/env bash
# or#855: install a pinned CI tool from its upstream release tarball.
#
# Replaces `go install <tool>@latest`, which was costing 47-61s PER JOB to
# compile a binary upstream already ships, and — worse — pinned nothing: a
# compromised or merely different upstream release landed straight in CI with
# no version and no integrity check. Measured on ubuntu-latest: `task` 61s,
# `gosec` 47s; this path is ~2s.
#
#   scripts/ci-install-tool.sh <task|gosec|sqlc> [dest-dir]   # default: bin/
#
# Prints the absolute path of the installed binary on stdout; everything else
# goes to stderr, so:  TASK="$(scripts/ci-install-tool.sh task)"
#
# BUMPING: change the version AND the sha256 together. Getting a checksum
# mismatch means the artifact changed under a tag — do not "fix" it by
# recomputing the digest without understanding why.
#
# gosec is deliberately pinned even though that freezes its rule set: the repo
# already pins golangci-lint and sqlc for the stated reason that a floating
# version silently changes what a gate means. Bump it on purpose and read the
# release notes. (govulncheck is pinned in the workflow instead — its
# vulnerability DB is fetched at run time, so the tool version does not freeze
# what it detects.)
set -euo pipefail
cd "$(dirname "$0")/.."

tool="${1:?usage: ci-install-tool.sh <task|gosec|sqlc> [dest-dir]}"
dest="${2:-bin}"

case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) ;;
    *) echo "ci-install-tool: only linux/amd64 is pinned here (got $(uname -sm)); install $tool yourself" >&2; exit 1 ;;
esac

case "$tool" in
    task)
        version="3.52.0"
        url="https://github.com/go-task/task/releases/download/v${version}/task_linux_amd64.tar.gz"
        # upstream task_checksums.txt
        sha256="02c679ffae53dca791804847d78b31731615894e292948397c971c87ac9e95bd"
        member="task"
        ;;
    gosec)
        version="2.28.0"
        url="https://github.com/securego/gosec/releases/download/v${version}/gosec_${version}_linux_amd64.tar.gz"
        # upstream gosec_2.28.0_checksums.txt
        sha256="d7882e505b1ff345d458bf0e893eec8019bc849f861ad73a212869540dd505ff"
        member="gosec"
        ;;
    sqlc)
        # Must track Taskfile.yaml's SQLC_VERSION.
        version="1.31.1"
        url="https://github.com/sqlc-dev/sqlc/releases/download/v${version}/sqlc_${version}_linux_amd64.tar.gz"
        # sqlc publishes no checksums file; digest recorded on first pin
        # (2026-07-29) and enforced from then on.
        sha256="497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354"
        member="sqlc"
        ;;
    *)
        echo "ci-install-tool: unknown tool '$tool'" >&2
        exit 2
        ;;
esac

mkdir -p "$dest"
out="$(cd "$dest" && pwd)/${member}-${version}"

if [ ! -x "$out" ]; then
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    echo "ci-install-tool: fetching ${tool} ${version}" >&2
    curl -fsSL -o "$tmp/archive.tar.gz" "$url"
    echo "${sha256}  ${tmp}/archive.tar.gz" | sha256sum -c - >&2
    tar -xzf "$tmp/archive.tar.gz" -C "$tmp" "$member"
    mv "$tmp/$member" "$out"
    chmod +x "$out"
fi

# Prove the thing we just installed actually runs, so a silently-broken binary
# fails here rather than looking like a clean gate downstream.
"$out" --version >/dev/null 2>&1 || "$out" version >/dev/null 2>&1 || {
    echo "ci-install-tool: installed ${tool} at ${out} does not execute" >&2
    exit 1
}

printf '%s\n' "$out"
