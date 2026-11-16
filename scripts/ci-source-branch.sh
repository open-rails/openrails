#!/usr/bin/env bash
# master takes pull requests only from the open release candidate, named for the
# version it becomes (rc/X.Y.Z), and from scripts/release (chore/release-vX.Y.Z),
# both in this repository.
set -euo pipefail
repository="${1:?repository}" head_repository="${2:?head repository}" head_ref="${3:?head ref}"
version='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
candidate="^rc/$version\$"
release="^chore/release-v$version(-[0-9A-Za-z.-]+)?\$"
if [[ "$head_repository" != "$repository" ]]; then
  reason="$head_repository:$head_ref is a fork branch"
elif [[ "$head_ref" =~ $candidate || "$head_ref" =~ $release ]]; then
  echo "Source branch $head_ref may merge into master."
  exit 0
else
  reason="$head_ref is neither rc/X.Y.Z nor chore/release-vX.Y.Z"
fi
echo "::error::master refuses this pull request: $reason. Retarget it to the open release candidate named in OPEN.md; master takes only that RC and scripts/release." >&2
exit 1
