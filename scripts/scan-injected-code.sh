#!/usr/bin/env bash
#
# scan-injected-code.sh — detect injected malware in tracked source files.
#
# Guards against the repo-injection worm (DPRK "Contagious Interview" /
# PolinRider class) that appended an obfuscated dropper to build-config files on
# infected contributor machines and rode into this repo inside otherwise normal
# commits (2025-11 .. 2026-05: vite/webpack/tailwind/postcss configs).
#
# The payload hides after a ~2800-space run on the last line of a config file,
# so it is invisible in an editor and in `git diff` without scrolling right.
# The obfuscator's variable names and version strings change between variants,
# so the load-bearing rules here are STRUCTURAL (whitespace runs, line length)
# rather than literal-signature matches.
#
# Usage:
#   scan-injected-code.sh [--all]              scan every tracked file (default)
#   scan-injected-code.sh --staged             scan staged files (pre-commit hook)
#   scan-injected-code.sh --range <rev-range>  scan files changed in a range (CI)
#   scan-injected-code.sh --files <path>...    scan the named paths verbatim
#   scan-injected-code.sh --self-test          run the detector against its fixtures
#
# Exits non-zero if anything is found. See docs/injection-scan.md.

set -euo pipefail

# --- tunables ----------------------------------------------------------------

MAX_SPACES=100        # rule 1: consecutive spaces (non-Markdown)
MAX_TABS=20           # rule 1: consecutive tabs
MAX_CONFIG_LINE=500   # rule 6: longest line allowed in a build config
DEAD_DROP_WINDOW=1000 # rule 4: chars allowed between a C2 host and an eval/spawn

DEAD_DROP_HOSTS='trongrid\.io|aptoslabs\.com|bsc-dataseed|bsc-rpc\.publicnode\.com|eth_getTransactionByHash'

# Excluded from every rule: the fixtures deliberately carry live signatures.
SKIP_ALL_RE='(^|/)scripts/testdata/injection-scan/'
# Excluded from the content rules (2-5) only: these two quote the signatures they
# describe. Both are still subject to the structural whitespace rule.
SKIP_CONTENT_RE='(^|/)(scripts/scan-injected-code\.sh|docs/injection-scan\.md)$'

self="${BASH_SOURCE[0]}"
findings=0

# --- argument parsing --------------------------------------------------------

usage() {
	sed -n '3,25p' "$self" | sed -e 's/^#\{0,1\} \{0,1\}//'
	exit 2
}

mode=all
range=""
declare -a explicit_files=()

while [ $# -gt 0 ]; do
	case "$1" in
	--all) mode=all ;;
	--staged) mode=staged ;;
	--range)
		mode=range
		range="${2:-}"
		[ -n "$range" ] || {
			echo "--range needs a rev-range (e.g. origin/master...HEAD)" >&2
			exit 2
		}
		shift
		;;
	--files)
		mode=files
		shift
		explicit_files=("$@")
		break
		;;
	--self-test) mode=self-test ;;
	-h | --help) usage ;;
	*)
		echo "unknown argument: $1" >&2
		usage
		;;
	esac
	shift
done

# --- reporting ---------------------------------------------------------------

# emit RULE FILE LINE DETAIL — detail is truncated so a payload never lands in a CI log.
emit() {
	local rule="$1" file="$2" line="$3" detail="$4"
	[ "${#detail}" -le 140 ] || detail="${detail:0:140}..."
	printf '%s:%s: [%s] %s\n' "$file" "$line" "$rule" "$detail"
	findings=$((findings + 1))
}

# grep_rule RULE MESSAGE PATTERN FILE... — one PCRE pass over the given files.
grep_rule() {
	local rule="$1" message="$2" pattern="$3"
	shift 3
	[ $# -gt 0 ] || return 0
	local hits file line text
	hits="$(printf '%s\0' "$@" |
		xargs -0 --no-run-if-empty grep -HnIP -e "$pattern" -- 2>/dev/null || true)"
	[ -n "$hits" ] || return 0
	while IFS= read -r hit; do
		file="${hit%%:*}"
		hit="${hit#*:}"
		line="${hit%%:*}"
		text="${hit#*:}"
		# Collapse padding so the detail shows the code, not the disguise.
		text="$(printf '%s' "$text" | sed -E -e 's/^[[:space:]]+//' -e 's/[[:space:]]{6,}/ ...pad... /g')"
		emit "$rule" "$file" "$line" "$message: $text"
	done <<<"$hits"
}

# --- file collection ---------------------------------------------------------

list_files() {
	case "$mode" in
	all) git ls-files -z ;;
	staged) git diff --cached --name-only --diff-filter=ACM -z ;;
	range) git diff --name-only --diff-filter=ACM -z "$range" ;;
	files) [ "${#explicit_files[@]}" -eq 0 ] || printf '%s\0' "${explicit_files[@]}" ;;
	esac
}

declare -a FILES=() CONTENT_FILES=() WS_FILES=() CONFIG_FILES=()
declare -a raw=()
mapfile -d '' -t raw < <(list_files)

# Explicit paths are scanned verbatim (no exclusions) so the self-test can point
# the detector at its own fixtures.
for f in ${raw[@]+"${raw[@]}"}; do
	[ -f "$f" ] || continue
	if [ "$mode" != files ] && [[ $f =~ $SKIP_ALL_RE ]]; then
		continue
	fi
	FILES+=("$f")
	if [ "$mode" = files ] || ! [[ $f =~ $SKIP_CONTENT_RE ]]; then
		CONTENT_FILES+=("$f")
	fi
	# Rule 1 excludes Markdown: padded Markdown tables legitimately carry long
	# space runs (a real doujins doc has a 166-space run).
	[[ $f =~ \.mdx?$ ]] || WS_FILES+=("$f")
	if [[ $f =~ (^|/)([^/]+\.config\.(js|cjs|mjs|ts)|(vite|webpack|tailwind|postcss|next)\.config\.[^/]+)$ ]]; then
		CONFIG_FILES+=("$f")
	fi
done

# --- self-test ---------------------------------------------------------------

if [ "$mode" = self-test ]; then
	fixtures="$(git rev-parse --show-toplevel)/scripts/testdata/injection-scan"
	[ -d "$fixtures" ] || {
		echo "self-test: fixtures missing at $fixtures" >&2
		exit 2
	}
	failed=0
	# check FIXTURE [EXPECTED_RULE...] — no expected rules means "must be clean".
	check() {
		local fixture="$1" out rc=0 rule missing=()
		shift
		out="$("$self" --files "$fixtures/$fixture" 2>&1)" || rc=$?
		if [ $# -eq 0 ]; then
			if [ "$rc" -eq 0 ]; then
				echo "ok   $fixture (clean)"
			else
				echo "FAIL $fixture: expected clean, got:" >&2
				echo "$out" >&2
				failed=1
			fi
			return 0
		fi
		for rule in "$@"; do
			grep -q "\[$rule\]" <<<"$out" || missing+=("$rule")
		done
		if [ "$rc" -ne 0 ] && [ "${#missing[@]}" -eq 0 ]; then
			echo "ok   $fixture ($*)"
		else
			echo "FAIL $fixture: expected [$*], missing [${missing[*]-}], exit $rc" >&2
			echo "$out" >&2
			failed=1
		fi
		return 0
	}

	check padded-dropper.config.js R1-whitespace R5-marker R6-config-line
	check eval-atob.js R2-eval-decode
	check detached-spawn.js R3-detached-spawn
	check dead-drop-c2.txt R4-dead-drop
	check padded-table.md
	check clean.config.ts
	# Regression guard for the calibration false positive: a vendored web3 SDK
	# bundle names chain RPC hosts but keeps no eval/spawn near them.
	check vendor-bundle-benign.js

	# The tracked tree itself must be clean, in both whole-tree and range mode.
	if "$self" --all >/dev/null 2>&1; then
		echo "ok   --all (tracked tree clean)"
	else
		echo "FAIL --all: findings in the tracked tree" >&2
		"$self" --all >&2 || true
		failed=1
	fi
	# Shallow CI checkouts have no HEAD~1; skip rather than fail.
	if ! git rev-parse --verify -q HEAD~1 >/dev/null; then
		echo "skip --range (no HEAD~1 in a shallow checkout)"
	elif "$self" --range HEAD~1...HEAD >/dev/null 2>&1; then
		echo "ok   --range (last commit clean)"
	else
		echo "FAIL --range HEAD~1...HEAD" >&2
		"$self" --range HEAD~1...HEAD >&2 || true
		failed=1
	fi

	if [ "$failed" -ne 0 ]; then
		echo "self-test FAILED" >&2
		exit 1
	fi
	echo "self-test passed"
	exit 0
fi

# --- rules -------------------------------------------------------------------

if [ "${#FILES[@]}" -eq 0 ]; then
	echo "injection scan: no files to scan"
	exit 0
fi

# Rule 1 — invisible padding. The most variant-proof signal there is: the
# dropper is pushed off-screen behind ~2800 spaces on a config's last line.
# grep -l pre-filters (and drops binaries) so the char-by-char awk pass that
# measures the run only ever touches files that already matched.
declare -a WS_HITS=()
if [ "${#WS_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t WS_HITS < <(
		printf '%s\0' "${WS_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -lIZP -e " {${MAX_SPACES},}|\t{${MAX_TABS},}" -- 2>/dev/null || true
	)
fi
for f in ${WS_HITS[@]+"${WS_HITS[@]}"}; do
	while IFS='	' read -r lineno spaces tabs; do
		emit R1-whitespace "$f" "$lineno" \
			"invisible padding: ${spaces}-space / ${tabs}-tab run (limits ${MAX_SPACES}/${MAX_TABS}) - a payload may be hidden past it"
	done < <(awk -v ms="$MAX_SPACES" -v mt="$MAX_TABS" '
		{
			sr = 0; tr = 0; maxs = 0; maxt = 0
			n = length($0)
			for (i = 1; i <= n; i++) {
				c = substr($0, i, 1)
				if (c == " ")       { sr++; tr = 0; if (sr > maxs) maxs = sr }
				else if (c == "\t") { tr++; sr = 0; if (tr > maxt) maxt = tr }
				else                { sr = 0; tr = 0 }
			}
			if (maxs >= ms || maxt >= mt) printf "%d\t%d\t%d\n", FNR, maxs, maxt
		}' "$f")
done

# Rule 2 — decode-and-execute on one line.
grep_rule R2-eval-decode \
	"eval() of decoded data" \
	'^(?=.*(?<![A-Za-z0-9_$])eval\s*\().*(?:atob\s*\(|Buffer\s*\.\s*from\s*\([^)]*base64)' \
	${CONTENT_FILES[@]+"${CONTENT_FILES[@]}"}

# Rule 3 — the persistence primitive: the dropper re-spawns itself detached from
# the build that started it. child_process must appear in the same file, so an
# ordinary `detached` identifier elsewhere cannot trip this.
declare -a DETACHED_SCOPE=()
if [ "${#CONTENT_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t DETACHED_SCOPE < <(
		printf '%s\0' "${CONTENT_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -lIZ -e 'child_process' -- 2>/dev/null || true
	)
fi
grep_rule R3-detached-spawn \
	"detached child_process spawn (persistence primitive)" \
	'detached\s*[:=]\s*(?:true|!0|1)\b' \
	${DETACHED_SCOPE[@]+"${DETACHED_SCOPE[@]}"}

# Rule 4 — blockchain dead-drop C2 resolution, corroborated by an execution
# primitive within ${DEAD_DROP_WINDOW} characters.
#
# A bare hostname is NOT enough: doujins-legacy legitimately ships the Blocto
# multi-chain wallet SDK, whose minified bundles mention bsc-dataseed. Nor is
# same-LINE co-occurrence enough - those bundles are a single 379 KB line, so
# every string in the SDK shares a line with every other string. What separates
# them from a dropper is distance: in the benign bundles the nearest
# execution-ish token ("detached") sits 61,100 characters from the hostname,
# while a dropper packs its C2 host and its eval/spawn into one compact blob.
#
# Path exclusions were rejected here: skipping public/js, dist and *.min.js
# would carve out exactly the kind of unreadable build artifact a payload most
# wants to live in.
declare -a HOST_FILES=()
if [ "${#CONTENT_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t HOST_FILES < <(
		printf '%s\0' "${CONTENT_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -lIZP -e "$DEAD_DROP_HOSTS" -- 2>/dev/null || true
	)
fi
for f in ${HOST_FILES[@]+"${HOST_FILES[@]}"}; do
	while IFS=$'\t' read -r kind lineno rest; do
		case "$kind" in
		HIT)
			dist="${rest%%$'\t'*}"
			snippet="$(printf '%s' "${rest#*$'\t'}" | sed -E 's/[[:space:]]{6,}/ ...pad... /g')"
			emit R4-dead-drop "$f" "$lineno" \
				"dead-drop C2 host ${dist} chars from an eval/spawn primitive: $snippet"
			;;
		WARN)
			# Visible but non-blocking: a hostname with no execution primitive
			# anywhere near it has been a vendored web3 bundle in every case seen.
			printf 'NOTE %s:%s: [R4-uncorroborated] dead-drop hostname with no eval/spawn within %s chars - vendored web3 SDK? verify once, then ignore\n' \
				"$f" "$lineno" "$DEAD_DROP_WINDOW"
			;;
		esac
	done < <(awk -v win="$DEAD_DROP_WINDOW" '
		function findall(s, re, arr,   n, off) {
			n = 0; off = 0
			while (match(s, re)) {
				n++; arr[n] = off + RSTART
				off += RSTART + RLENGTH - 1
				s = substr(s, RSTART + RLENGTH)
				if (RLENGTH == 0) break
			}
			return n
		}
		BEGIN {
			hostre = "trongrid\\.io|aptoslabs\\.com|bsc-dataseed|bsc-rpc\\.publicnode\\.com|eth_getTransactionByHash"
			markre = "(^|[^A-Za-z0-9_$])(eval|atob)[ \t]*\\(|child_process|detached[ \t]*[:=]|Buffer[ \t]*\\.[ \t]*from[ \t]*\\([^)]*base64"
		}
		{
			nh = findall($0, hostre, hp)
			if (nh == 0) next
			nm = findall($0, markre, mp)
			best = -1
			for (i = 1; i <= nh; i++)
				for (j = 1; j <= nm; j++) {
					d = hp[i] - mp[j]; if (d < 0) d = -d
					if (best < 0 || d < best) { best = d; bi = i }
				}
			if (best >= 0 && best <= win)
				printf "HIT\t%d\t%d\t%s\n", FNR, best, substr($0, (hp[bi] > 60 ? hp[bi] - 60 : 1), 200)
			else
				printf "WARN\t%d\n", FNR
		}' "$f")
done

# Rule 5 — known obfuscator markers, generalised past the observed literals
# (the version strings and table names change between variants). Fast path
# only; rules 1 and 6 are the net.
grep_rule R5-marker \
	"known dropper obfuscator marker" \
	'_\$_[0-9a-f]{4}|global\s*(?:\[\s*["\x27][^"\x27]{1,8}["\x27]\s*\]|\.\s*\w{1,8})\s*=\s*["\x27][0-9]+-[0-9]+-[0-9]+["\x27]' \
	${CONTENT_FILES[@]+"${CONTENT_FILES[@]}"}

# Rule 6 — code appended past the last legitimate statement of a build config.
# Practically: a build config has no business carrying a very long line (the
# longest legitimate one across these repos is 97 chars).
for cfg in ${CONFIG_FILES[@]+"${CONFIG_FILES[@]}"}; do
	read -r len lineno < <(
		awk 'length($0) > m { m = length($0); n = FNR } END { print m + 0, n + 0 }' "$cfg"
	)
	if [ "$len" -gt "$MAX_CONFIG_LINE" ]; then
		emit R6-config-line "$cfg" "$lineno" \
			"build config carries a ${len}-char line (limit ${MAX_CONFIG_LINE}) - code appended past the last statement?"
	fi
done

# --- verdict -----------------------------------------------------------------

if [ "$findings" -gt 0 ]; then
	cat >&2 <<-EOF

		injection scan FAILED: $findings finding(s).

		These are the signatures of a repo-injection worm that appends an obfuscated
		dropper to build-config files. Do NOT run, build, or import the flagged file.
		Inspect it with 'cat -A' or 'git diff' and scroll right; if the hit is real, the
		machine that produced the commit is compromised and needs to be rebuilt.

		False positive? Tune the specific rule in scripts/scan-injected-code.sh - do not
		add a blanket skip.
	EOF
	exit 1
fi

echo "injection scan clean (${#FILES[@]} files)"
