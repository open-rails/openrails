#!/usr/bin/env bash
#
# install-git-hooks.sh — symlink this repo's tracked git hooks into .git/hooks.
#
# Run once per clone:
#
#   ./scripts/install-git-hooks.sh
#
# Installs a pre-commit hook that runs scripts/scan-injected-code.sh over the
# staged files (see docs/injection-scan.md). The hook is a symlink to the
# tracked scripts/hooks/pre-commit, so it stays current with the repo.
#
# An existing, unrelated hook is never overwritten silently: it is moved aside
# to <hook>.local and the installer tells you where it went.

set -euo pipefail

root="$(git rev-parse --show-toplevel)"
hooks_dir="$(git rev-parse --git-path hooks)"
case "$hooks_dir" in
/*) ;;
*) hooks_dir="$root/$hooks_dir" ;;
esac

mkdir -p "$hooks_dir"

for hook in pre-commit; do
	src="$root/scripts/hooks/$hook"
	dst="$hooks_dir/$hook"

	[ -f "$src" ] || {
		echo "missing tracked hook: $src" >&2
		exit 1
	}
	chmod +x "$src"

	if [ -L "$dst" ] && [ "$(readlink -f "$dst")" = "$(readlink -f "$src")" ]; then
		echo "already installed: $dst"
		continue
	fi

	if [ -e "$dst" ] || [ -L "$dst" ]; then
		mv -- "$dst" "$dst.local"
		echo "moved your existing $hook hook to $dst.local - re-add its contents by hand if you still need it"
	fi

	ln -s -- "$src" "$dst"
	echo "installed: $dst -> $src"
done

echo
echo "Verify with: ./scripts/scan-injected-code.sh --self-test"
