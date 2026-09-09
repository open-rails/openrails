#!/usr/bin/env bash
#
# install-git-hooks.sh — symlink this repo's tracked git hooks into .git/hooks.
#
# Run once per clone:
#
#   ./scripts/install-git-hooks.sh
#
# Installs every hook tracked in scripts/hooks/. Each hook is a symlink to its
# tracked source, so it stays current with the repo.
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

found_hook=false
for src in "$root"/scripts/hooks/*; do
	[ -f "$src" ] || continue
	found_hook=true
	hook="$(basename "$src")"
	dst="$hooks_dir/$hook"
	chmod +x "$src"

	# Links created below always use the absolute source path. Comparing that
	# exact value is portable to macOS, whose readlink has no -f flag.
	if [ -L "$dst" ] && [ "$(readlink "$dst")" = "$src" ]; then
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

[ "$found_hook" = true ] || {
	echo "no tracked hooks found under $root/scripts/hooks" >&2
	exit 1
}

echo
echo "Verify with: task doctor"
