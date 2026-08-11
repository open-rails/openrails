#!/usr/bin/env bash
# or#919 (ported from tensorhub th#1790, `576f0e31`): does this change claim a
# migration number that `origin/master` has ALREADY used for a different file?
#
# THE FAILURE THIS CATCHES — AND THIS REPO IS WHERE IT ACTUALLY HAPPENED.
# migratekit keys the applied-migrations ledger by the numeric PREFIX, so two
# files claiming `0005` are ONE migration to every database, and LoadFromFS
# refuses the duplicate pair — before applying anything. Each PR is green
# alone; the break exists only in the merge. Measured here 2026-08-11: or#908
# (PR #275) and or#911 (PR #276) each shipped a `0005`, merged minutes apart,
# and every boot and every `migrate` on master failed until hotfix PR #278
# (`8639ce85`) renumbered one of them to `0006`.
#
# WHY A BRANCH-TIME ANSWER IS NOT ENOUGH. Any check computed from the tree it
# was handed — the lane, or the pull_request merge ref as GitHub last computed
# it — is a snapshot of a base that keeps moving, and GitHub does not re-run a
# check when the base moves under an open PR. Both colliding PRs here read
# `mergeStateStatus: CLEAN` right up to the merge.
#
# So this gate resolves `origin/master` ITSELF, over the network, at the moment
# it runs. Its answer is true as of its own start and no earlier — which is the
# most any per-run check can honestly claim, and is why it prints the sha it
# measured and says so in the job summary.
#
# CONSEQUENTLY: RE-RUN IT IMMEDIATELY BEFORE YOU MERGE. A green run from before
# a sibling landed proves nothing about the merge you are about to press. The
# structural version of that discipline is the ruleset's "require branches to be
# up to date before merging", which forces the re-run; see or#919.
#
# WHAT IT DOES NOT CHECK. Duplicates WITHIN one tree are migratekit's own
# LoadFromFS refusal (v1.4.0+), and lock-safety is scripts/migration-lint.sh.
# This repo does not enforce a gapless chain (migratekit v1.4.0, no
# CheckChain/WithStrictOrdering), so gaps are not this script's business either.
# The one question here is the one nothing else answers: what does master hold
# RIGHT NOW.
#
# NO DATABASE, NO CONTAINERS, NO GO BUILD. One shallow fetch and two listings,
# so it costs a couple of seconds and can be re-run freely.
#
# SOFT ON INFRASTRUCTURE, HARD ON COLLISIONS. If master cannot be resolved the
# script says UNVERIFIED loudly — annotation, log and summary — and exits 0. A
# PR is not broken because a fetch was. Only a real collision exits non-zero.
#
# Usage: scripts/migration-prefix-collision.sh
# Env:
#   MIGRATION_PREFIX_REMOTE  remote to resolve master from   (default: origin)
#   MIGRATION_PREFIX_BRANCH  branch that owns the numbers    (default: master)
#   MIGRATION_PREFIX_NO_FETCH=1  read the already-fetched ref instead of the
#                            network (offline local runs; CI always fetches)
#   MIGRATION_PREFIX_SQUASH=1  a deliberate chain squash (the or#893 shape)
#                            rewrites every number, so the comparison is
#                            meaningless — skip. The common squash shape is
#                            also auto-detected below.
set -euo pipefail
cd "$(dirname "$0")/.."

DIR="migrations/postgres"
REMOTE="${MIGRATION_PREFIX_REMOTE:-origin}"
BRANCH="${MIGRATION_PREFIX_BRANCH:-master}"
REF="refs/remotes/$REMOTE/$BRANCH"
LABEL="migration-prefix"

say() { echo "[$LABEL] $*"; }

# summary appends to the job summary when GitHub gives us one, so the verdict is
# readable without opening the log.
summary() {
  [ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
  printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY"
}

# unverified: the measurement could not be taken. Loud, annotated, exit 0. A
# silent green would be worse than a warning, which is why the word UNVERIFIED
# reaches every channel this job writes to.
unverified() {
  echo "::warning title=migration prefix UNVERIFIED::$*"
  say "UNVERIFIED — $*"
  say "the PR is NOT failed for this: nothing was measured, so nothing was proven."
  summary "### migration prefix: :warning: UNVERIFIED"
  summary ""
  summary "$*"
  summary ""
  summary "No collision check was performed. This is an infrastructure result, not a verdict on the change."
  exit 0
}

# rows_of prints "<prefix> <slug> <path>" for every migration file in a tree.
# The direction suffix is dropped from the key on purpose: an up/down PAIR is
# two files and ONE migration, and must never look like two claims on a number.
rows_of() {
  git ls-tree -r --name-only "$1" -- "$DIR/" 2>/dev/null \
    | sed -n -E "s#^$DIR/([0-9]{4})_([a-z0-9_]+)\.(up|down)\.sql\$#\1 \2 &#p" \
    | sort -u
}

# The local side reads the WORKING TREE, not HEAD, so a local run answers for a
# migration you have written but not yet committed — which is the moment the
# answer is cheapest to act on. In CI the two are identical.
local_rows() {
  git ls-files --cached --others --exclude-standard -- "$DIR/" 2>/dev/null \
    | sed -n -E "s#^$DIR/([0-9]{4})_([a-z0-9_]+)\.(up|down)\.sql\$#\1 \2 &#p" \
    | sort -u
}

head_rows="$(local_rows || true)"

if [ -z "$head_rows" ]; then
  say "no migrations in this tree — nothing can collide."
  summary "### migration prefix: :white_check_mark: no migrations in this change"
  exit 0
fi

# --- resolve master FRESH -----------------------------------------------------
if [ "${MIGRATION_PREFIX_NO_FETCH:-0}" = "1" ]; then
  git rev-parse --verify -q "$REF" >/dev/null \
    || unverified "MIGRATION_PREFIX_NO_FETCH=1 but $REF is not present locally."
else
  fetch_log="$(mktemp)"
  # --depth=1: only the tip tree is ever read, so there is no reason to pay for
  # history. On an already-complete clone git ignores the shallow hint.
  if ! git fetch --no-tags --quiet --depth=1 "$REMOTE" "+refs/heads/$BRANCH:$REF" 2>"$fetch_log"; then
    if ! git fetch --no-tags --quiet "$REMOTE" "+refs/heads/$BRANCH:$REF" 2>>"$fetch_log"; then
      err="$(tr '\n' ' ' <"$fetch_log" 2>/dev/null || true)"
      rm -f "$fetch_log"
      unverified "could not fetch $REMOTE/$BRANCH: ${err:-unknown fetch failure}"
    fi
  fi
  rm -f "$fetch_log"
fi

master_sha="$(git rev-parse --verify -q "$REF" || true)"
[ -n "$master_sha" ] || unverified "$REF did not resolve after fetching $REMOTE/$BRANCH."

master_rows="$(rows_of "$REF" || true)"
if [ -z "$master_rows" ]; then
  unverified "$REMOTE/$BRANCH at ${master_sha:0:12} lists no migrations under $DIR/ — refusing to read that as a clean namespace."
fi

say "$REMOTE/$BRANCH resolved fresh at ${master_sha:0:12} — $(printf '%s\n' "$master_rows" | wc -l | tr -d '[:space:]') migration files"

# --- chain squash: every number is rewritten, so the comparison is meaningless -
head_ups="$(printf '%s\n' "$head_rows" | grep -c '\.up\.sql$' || true)"
master_ups="$(printf '%s\n' "$master_rows" | grep -c '\.up\.sql$' || true)"
if [ "${MIGRATION_PREFIX_SQUASH:-0}" = "1" ] || { [ "$head_ups" = "1" ] && [ "$master_ups" -gt 1 ]; }; then
  say "chain-squash shape (this tree has $head_ups up-migration, $BRANCH has $master_ups) — a prefix comparison does not apply."
  say "a squash rewrites every number by design (or#893 is this repo's precedent); reviewing the squash itself is the PR reviewer's job, not this gate's."
  summary "### migration prefix: :white_check_mark: chain squash — not applicable"
  exit 0
fi

# --- the one question ---------------------------------------------------------
# A number collides when THIS tree claims prefix P with slug S, master already
# claims P, and S is not among the slugs master claims it with.
#
# Written that way rather than "S differs from master's slug" deliberately: when
# master's own chain holds P twice (exactly what master looked like between
# PR #276 and hotfix PR #278), the repair renames ONE of the two, and the file
# that KEEPS its number is still one of master's own files — it must not be
# reported as this change's fault. Set membership passes the repair; the naive
# form fails the very change that fixes master.
collisions=""
while read -r prefix slug path; do
  [ -n "$prefix" ] || continue
  master_slugs="$(printf '%s\n' "$master_rows" | awk -v p="$prefix" '$1 == p { print $2 }' | sort -u)"
  [ -n "$master_slugs" ] || continue                              # master never used this number
  printf '%s\n' "$master_slugs" | grep -qx -- "$slug" && continue # same migration, already there
  master_paths="$(printf '%s\n' "$master_rows" | awk -v p="$prefix" '$1 == p { print $3 }' | sort -u | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
  collisions="${collisions}${prefix}|${path}|${master_paths}
"
done <<<"$head_rows"

if [ -z "$collisions" ]; then
  say "OK — no number in this tree is claimed by a different file on $REMOTE/$BRANCH (${master_sha:0:12})."
  summary "### migration prefix: :white_check_mark: no collision with \`$REMOTE/$BRANCH\` @ \`${master_sha:0:12}\`"
  summary ""
  summary "Resolved over the network at the start of this run. **A green result goes stale the moment a sibling merges a migration** — re-run this job immediately before merging."
  exit 0
fi

# --- report -------------------------------------------------------------------
# Annotations on stdout (that is the stream GitHub parses workflow commands
# from); the human report on stderr, next to the failure.
printf '%s' "$collisions" | while IFS='|' read -r prefix path master_paths; do
  [ -n "$prefix" ] || continue
  echo "::error title=migration prefix $prefix is already used on $BRANCH::$path collides with $master_paths"
done

{
  echo
  echo "$LABEL: this change claims a migration number that $REMOTE/$BRANCH has already used."
  echo
  printf '%s' "$collisions" | while IFS='|' read -r prefix path master_paths; do
    [ -n "$prefix" ] || continue
    echo "  prefix $prefix"
    echo "    this change:      $path"
    echo "    $REMOTE/$BRANCH:  $master_paths"
  done
  echo
  echo "  migratekit keys the applied-migrations ledger by the PREFIX, so those files are"
  echo "  ONE migration to every database. LoadFromFS refuses the duplicate pair before"
  echo "  applying anything — so merging this does not write a wrong row, it produces a"
  echo "  tree where every boot and every \`migrate\` fails outright (this repo's or#908/"
  echo "  or#911 outage, repaired by PR #278)."
  echo
  echo "  Rebase and renumber (BOTH directions of the pair, if a .down.sql exists):"
  echo "    git fetch origin && git rebase origin/master"
  echo "    git mv $DIR/NNNN_<slug>.up.sql   $DIR/<next>_<slug>.up.sql"
  echo "    git mv $DIR/NNNN_<slug>.down.sql $DIR/<next>_<slug>.down.sql"
  echo "    scripts/migration-prefix-collision.sh"
  echo
  echo "  Measured against $REMOTE/$BRANCH at $master_sha, resolved at the start of this run."
} 1>&2

summary "### migration prefix: :x: COLLISION with \`$REMOTE/$BRANCH\` @ \`${master_sha:0:12}\`"
summary ""
summary "| prefix | this change | on \`$REMOTE/$BRANCH\` |"
summary "| --- | --- | --- |"
printf '%s' "$collisions" | while IFS='|' read -r prefix path master_paths; do
  [ -n "$prefix" ] || continue
  summary "| \`$prefix\` | \`$path\` | \`$master_paths\` |"
done
summary ""
summary "migratekit keys the ledger by the prefix, so these are one migration to every database and the pair is refused at load — every boot and every \`migrate\` on this tree would fail (the or#908/or#911 outage shape). Rebase onto \`$BRANCH\` and renumber both directions of the pair."

exit 1
