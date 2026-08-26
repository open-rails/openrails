#!/usr/bin/env bash
#
# scan-injected-code.sh — detect injected malware in tracked source files.
#
# Guards against the repo-injection worm (DPRK "Contagious Interview" /
# PolinRider class) that appended an obfuscated dropper to build-config files on
# infected contributor machines and rode into this repo inside otherwise normal
# commits (2025-11 .. 2026-05: vite/webpack/tailwind/postcss configs, and
# frontend/scripts/setup-husky.mjs in cozy-art).
#
# The payload hides after a ~2800-space run on the last line of a config file,
# so it is invisible in an editor and in `git diff` without scrolling right. The
# obfuscator's variable names and version strings change between variants, so
# the load-bearing rules here are STRUCTURAL (whitespace, line length, file
# size, string length) rather than literal-signature matches.
#
# Usage:
#   scan-injected-code.sh [--all]              scan every tracked file (default)
#   scan-injected-code.sh --all --include-untracked
#                                              ...plus untracked, non-ignored files
#   scan-injected-code.sh --staged             scan staged blobs (pre-commit hook)
#   scan-injected-code.sh --range <rev-range>  scan blobs changed in a range (CI)
#   scan-injected-code.sh --files <path>...    scan the named paths verbatim
#   scan-injected-code.sh --self-test          run the detector against its fixtures
#
# Exits non-zero if anything is found. See docs/injection-scan.md.

set -euo pipefail
export LC_ALL=C # byte-wise matching; grep -P must not choke on invalid UTF-8

# --- tunables ----------------------------------------------------------------

MAX_SPACES=100             # rule 1: consecutive spaces (non-Markdown)
MAX_TABS=20                # rule 1: consecutive tabs
MAX_CONFIG_LINE=250        # rule 6: longest line allowed in a build config
MAX_CONFIG_INTERIOR_WS=40  # rule 7: whitespace chars after a config line's first non-blank
MAX_UNICODE_WS_RUN=20      # rule 8: non-ASCII whitespace run allowed outside configs
MAX_CONFIG_BYTES=8192      # rule 9: build-config size cap (raise for a big hand-written config)
MAX_CONFIG_STRING=200      # rule 10: longest string literal allowed in a build config
DEAD_DROP_WINDOW=1000      # rule 4: chars allowed between a C2 host and an eval/spawn
MIN_B64_LITERAL=16         # rule 12: shortest base64 literal worth decoding
MAX_DECODE_BYTES=65536     # rule 12: bytes of a decoded literal that get matched
MAX_TRAILING_GAP=40        # rule 14: whitespace gap allowed inside a file's last line

DEAD_DROP_HOSTS='trongrid\.io|aptoslabs\.com|bsc-dataseed|bsc-rpc\.publicnode\.com|eth_getTransactionByHash'

# Obfuscator markers, generalised past the observed literals (version strings and
# table names change between variants). Used by rules 5 and 12.
#
# The key name is a WILDCARD, not a list - global.i, global['_V'] and global.o
# have all been seen in the wild. The version string is N-N-N followed by any
# number of further '-<alnum>' segments: '5-3-160', '5-3-160-du' and
# '5-3-361-du' are one family. That generalisation is what catches the
# base64-encapsulated variant, whose ONLY cleartext leak is global.o='5-3-160-du'
# (its _$_bb1a table lives inside the encoded blob, invisible to a plaintext scan).
MARKER_RE='_\$_[0-9a-f]{4}|global\s*(?:\[\s*["\x27][^"\x27]{1,16}["\x27]\s*\]|\.\s*[A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*["\x27][0-9]+(?:-[0-9A-Za-z]+){2,}["\x27]'

# Rule 12 — what a decoded literal must contain to be a finding. A dead-drop
# host, a chain account address, any URL, or a marker. "Decodes cleanly" alone
# is not enough: legitimate web code base64s plenty of harmless data.
DECODED_INTERESTING="${DEAD_DROP_HOSTS}|0x[0-9a-fA-F]{40,}|https?://|${MARKER_RE}"

# Non-ASCII whitespace / invisibles, as raw UTF-8 bytes so the pattern works on
# any file: U+00A0 U+1680 U+2000-U+200B U+202F U+205F U+2060 U+3000 U+FEFF.
UNICODE_WS='(?:\xC2\xA0|\xE1\x9A\x80|\xE2\x80[\x80-\x8B\xAF]|\xE2\x81[\x9F\xA0]|\xE3\x80\x80|\xEF\xBB\xBF)'

# Rule 11 scope: a NUL byte is normal in a binary, never in these.
SOURCE_EXT_RE='\.(js|jsx|ts|tsx|mjs|cjs|go|php|py|sh)$'

# Build configs — the worm's target class. Deliberately wide: setup-husky.mjs
# and .husky/* were hit in the wild and match no *.config.* pattern. A bare
# `*rc.js` alternative was REJECTED: it matches a vendored
# public/vendor/codemirror/mode/mirc/mirc.js in the legacy trees.
CONFIG_RE='(^|/)([^/]+\.config\.[A-Za-z]+|(vite|webpack|tailwind|postcss|next|rollup|svelte|astro|nuxt)\.config\.[^/]+|(gulpfile|Gruntfile)\.[A-Za-z]+|setup-[^/]+\.(js|mjs|cjs|ts))$|(^|/)\.husky/[^/]+$'

# Excluded from every rule: the fixtures deliberately carry live signatures.
SKIP_ALL_RE='(^|/)scripts/testdata/injection-scan/'
# Excluded from the content rules (2-5) only: these two quote the signatures they
# describe. Both are still subject to the structural whitespace rule.
SKIP_CONTENT_RE='(^|/)(scripts/scan-injected-code\.sh|docs/injection-scan\.md)$'

self="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")"
findings=0

# --- argument parsing --------------------------------------------------------

usage() {
	sed -n '3,26p' "$self" | sed -e 's/^#\{0,1\} \{0,1\}//'
	exit 2
}

mode=all
range=""
include_untracked=0
declare -a explicit_files=()

while [ $# -gt 0 ]; do
	case "$1" in
	--all) mode=all ;;
	--include-untracked) include_untracked=1 ;;
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
	all)
		git ls-files -z
		if [ "$include_untracked" -eq 1 ]; then
			git ls-files -z --others --exclude-standard
		fi
		;;
	staged) git diff --cached --name-only --diff-filter=ACM -z ;;
	range) git diff --name-only --diff-filter=ACM -z "$range" ;;
	files) [ "${#explicit_files[@]}" -eq 0 ] || printf '%s\0' "${explicit_files[@]}" ;;
	esac
}

declare -a FILES=() CONTENT_FILES=() WS_FILES=() CONFIG_FILES=() SOURCE_FILES=() MANIFEST_FILES=()
declare -a raw=()
mapfile -d '' -t raw < <(list_files)

# --staged / --range must read the BLOB, not the working tree: `git add <bad
# file>` followed by restoring a clean copy on disk otherwise sails through the
# hook while the flagged blob is what actually gets committed. Materialise the
# blobs under a temp root and scan there, keeping repo-relative paths so the
# report still names the real file.
TMPROOT=""
cleanup() { [ -z "$TMPROOT" ] || rm -rf -- "$TMPROOT"; }
trap cleanup EXIT

materialize() {
	local rev="$1" f
	TMPROOT="$(mktemp -d "${TMPDIR:-/tmp}/injscan.XXXXXX")"
	for f in ${raw[@]+"${raw[@]}"}; do
		mkdir -p -- "$TMPROOT/$(dirname -- "$f")"
		if ! git show "$rev:$f" >"$TMPROOT/$f" 2>/dev/null; then
			rm -f -- "$TMPROOT/$f"
			echo "injection scan: no blob for $f at '${rev:-<index>}' - skipped" >&2
		fi
	done
	cd -- "$TMPROOT"
}

case "$mode" in
staged) materialize "" ;;
range)
	# Scan the tip side of the range: A...B / A..B -> B, bare rev -> itself.
	case "$range" in
	*...*) tip="${range##*...}" ;;
	*..*) tip="${range##*..}" ;;
	*) tip="$range" ;;
	esac
	materialize "${tip:-HEAD}"
	;;
esac

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
	if [[ $f =~ $CONFIG_RE ]]; then CONFIG_FILES+=("$f"); fi
	if [[ $f =~ $SOURCE_EXT_RE ]]; then SOURCE_FILES+=("$f"); fi
	if [[ $f =~ (^|/)manifest\.json$ ]]; then MANIFEST_FILES+=("$f"); fi
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
	# Rules 7-11.
	check interior-padded.config.js R7-config-interior-ws
	check unicode-padded.config.js R8-config-unicode-ws
	check unicode-run.js R8-unicode-ws-run
	check oversized.config.js R9-config-size
	check long-string.config.js R10-config-string
	check nul-byte.js R11-nul-byte
	check decoded-c2.js R12-decoded-payload
	# The 2026-08 field variants: a dropper in ordinary source (not a config),
	# and the base64-encapsulated form whose only cleartext leak is the marker.
	check appended-tail.ts R14-trailing-append
	check b64-encapsulated.js R2-eval-decode R5-marker R12-decoded-payload
	check tampered-extension/manifest.json R13-manifest-meta
	# Low-noise guards for rules 12 and 13: ordinary atob of harmless data, and a
	# wallet manifest whose client_id sits in oauth2, not in a __meta container.
	check benign-atob.js
	check benign-extension/manifest.json
	# Widened config file set: these paths are configs now, and each carries a
	# 300-char line that only R6 can see.
	check setup-husky.mjs R6-config-line
	check gulpfile.js R6-config-line
	# The bare-`*rc.js` alternative that was rejected: a vendored CodeMirror mode
	# named mirc.js is NOT a build config and must stay clean.
	check mirc.js

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

# Rule 14 — code appended past a file's terminal statement, ANY file type. The
# config-scoped rules (6, 7, 9, 10, and the strict form of 8) are blind to the
# two 2026-08 samples that landed the dropper in ordinary application source:
# bench/surrealdb-ws-bench/bench.ts (after `main();`) and source/index.js (after
# `handleErrors(main)();`). Both share one shape the whole class has: the file's
# LAST line ends its real code, then a whitespace gap, then more code.
#
# Anchoring to the last line is what makes this free. Measured across all seven
# trees: 0 files. The same gap test applied to EVERY line costs 9/1/15/11/1/3/2
# false positives (aligned trailing comments in Go and PHP), and a
# total-whitespace variant costs up to 158 (minified bundles), so neither ships.
declare -a GAP_CANDIDATES=()
if [ "${#WS_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t GAP_CANDIDATES < <(
		printf '%s\0' "${WS_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -lIZP -e "[^ \t][ \t]{${MAX_TRAILING_GAP},}[^ \t]" -- 2>/dev/null || true
	)
fi
for f in ${GAP_CANDIDATES[@]+"${GAP_CANDIDATES[@]}"}; do
	while IFS='	' read -r lineno gap; do
		emit R14-trailing-append "$f" "$lineno" \
			"code appended past the last statement: ${gap}-char whitespace gap inside the file's final line (limit ${MAX_TRAILING_GAP})"
	done < <(awk -v mg="$MAX_TRAILING_GAP" '
		$0 ~ /[^ \t\r]/ { last = $0; lastno = FNR }
		END {
			if (lastno == 0) exit
			run = 0; maxrun = 0; seen = 0
			n = length(last)
			for (i = 1; i <= n; i++) {
				c = substr(last, i, 1)
				if (c == " " || c == "\t") { if (seen) run++ }
				else { if (seen && run > maxrun) maxrun = run; run = 0; seen = 1 }
			}
			if (maxrun >= mg) printf "%d\t%d\n", lastno, maxrun
		}' "$f")
done

# Rule 2 — decode-and-execute on one line. Deliberately NOT adjacency: the
# lookahead finds eval( anywhere on the line and the body finds the decoder
# anywhere on it, so eval("prefix"+atob('...')) is caught like eval(atob(...)).
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
# only; the structural rules are the net.
grep_rule R5-marker \
	"known dropper obfuscator marker" \
	"$MARKER_RE" \
	${CONTENT_FILES[@]+"${CONTENT_FILES[@]}"}

# Rules 6, 7, 9, 10 — the build-config envelope. A build config is small,
# hand-written and short-lined; every dimension a dropper inflates is capped.
for cfg in ${CONFIG_FILES[@]+"${CONFIG_FILES[@]}"}; do
	# Rule 6 — code appended past the last legitimate statement. The longest
	# legitimate config line across these repos is 199 chars (cozy-art
	# frontend/vite.config.ts:50).
	read -r len lineno < <(
		awk 'length($0) > m { m = length($0); n = FNR } END { print m + 0, n + 0 }' "$cfg"
	)
	if [ "$len" -gt "$MAX_CONFIG_LINE" ]; then
		emit R6-config-line "$cfg" "$lineno" \
			"build config carries a ${len}-char line (limit ${MAX_CONFIG_LINE}) - code appended past the last statement?"
	fi

	# Rule 7 — interior whitespace. Padding-shape independent: it counts every
	# blank AFTER the line's first non-blank, so chunked padding (99 spaces +
	# /**/ + 99 spaces ...) and mixed space/tab runs cannot slip under a
	# run-length limit. Highest legitimate value measured is 31.
	while IFS='	' read -r wsline wscount; do
		emit R7-config-interior-ws "$cfg" "$wsline" \
			"build config line carries ${wscount} interior whitespace chars (limit ${MAX_CONFIG_INTERIOR_WS}) - padding hiding appended code?"
	done < <(awk -v mw="$MAX_CONFIG_INTERIOR_WS" '
		{
			s = $0
			sub(/^[[:space:]]+/, "", s)
			n = 0
			for (i = 1; i <= length(s); i++) {
				c = substr(s, i, 1)
				if (c == " " || c == "\t" || c == "\r" || c == "\f" || c == "\v") n++
			}
			if (n >= mw) printf "%d\t%d\n", FNR, n
		}' "$cfg")

	# Rule 9 — size cap. A hand-written build config is small; a dropper is not.
	bytes="$(wc -c <"$cfg")"
	if [ "$bytes" -gt "$MAX_CONFIG_BYTES" ]; then
		emit R9-config-size "$cfg" 1 \
			"build config is ${bytes} bytes (limit ${MAX_CONFIG_BYTES}) - raise MAX_CONFIG_BYTES only for a config you have read end to end"
	fi
done

# Rule 8 (configs) — ANY non-ASCII whitespace / invisible character. A build
# config has no business containing one, and 400 no-break spaces pad exactly
# like 400 spaces while sliding under a space-run rule.
grep_rule R8-config-unicode-ws \
	"non-ASCII whitespace in a build config" \
	"$UNICODE_WS" \
	${CONFIG_FILES[@]+"${CONFIG_FILES[@]}"}

# Rule 10 — an over-long string literal in a build config: the encoded payload
# table, wrapped across short lines so no single line trips rule 6.
grep_rule R10-config-string \
	"string literal over ${MAX_CONFIG_STRING} chars in a build config" \
	"(?:\x27[^\x27]{$((MAX_CONFIG_STRING + 1)),}\x27|\"[^\"]{$((MAX_CONFIG_STRING + 1)),}\"|\x60[^\x60]{$((MAX_CONFIG_STRING + 1)),}\x60)" \
	${CONFIG_FILES[@]+"${CONFIG_FILES[@]}"}

# Rule 8 (global) — the same invisibles anywhere else, but as a RUN. "Any
# occurrence" tree-wide costs 34 false positives in minified CSS/JS (real
# no-break spaces inside legitimate content strings); a run of
# ${MAX_UNICODE_WS_RUN} is padding, not content. Carriage returns pad too.
grep_rule R8-unicode-ws-run \
	"run of non-ASCII whitespace (padding)" \
	"${UNICODE_WS}{${MAX_UNICODE_WS_RUN},}|\r{${MAX_UNICODE_WS_RUN},}" \
	${WS_FILES[@]+"${WS_FILES[@]}"}

# Rule 11 — a NUL byte in a source file. Not just an oddity: `grep -I` treats
# such a file as binary and skips it, which silently disables rules 1-5 and 8
# for it. Scoped to source extensions - unscoped it fires on every legitimate
# binary in the tree.
declare -a NUL_HITS=()
if [ "${#SOURCE_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t NUL_HITS < <(
		printf '%s\0' "${SOURCE_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -laZP -e '\x00' -- 2>/dev/null || true
	)
fi
for f in ${NUL_HITS[@]+"${NUL_HITS[@]}"}; do
	emit R11-nul-byte "$f" 1 \
		"NUL byte in a source file - it reads as binary, so the text rules skip it; inspect with 'xxd'"
done

# Rule 12 — decode, THEN match. Rules 4 and 5 read plaintext; the fake-Phantom
# wallet stealer (2025-12) kept its C2 in
# atob("<base64 of an aptos fullnode account URL>") and so scored clean. Decode
# every base64 literal handed to atob()/Buffer.from(...,'base64') and re-run the
# dead-drop / marker patterns against the plaintext. In a build config ANY
# base64-shaped literal is decoded: a config has no legitimate reason to carry one.
declare -a B64_SCOPE=()
if [ "${#CONTENT_FILES[@]}" -gt 0 ]; then
	mapfile -d '' -t B64_SCOPE < <(
		printf '%s\0' "${CONTENT_FILES[@]}" |
			xargs -0 --no-run-if-empty grep -lIZP -e 'atob\s*\(|base64' -- 2>/dev/null || true
	)
fi
declare -A DECODE_SEEN=()
declare -a DECODE_FILES=()
for f in ${B64_SCOPE[@]+"${B64_SCOPE[@]}"} ${CONFIG_FILES[@]+"${CONFIG_FILES[@]}"}; do
	if [ -z "${DECODE_SEEN[$f]:-}" ]; then
		DECODE_SEEN["$f"]=1
		DECODE_FILES+=("$f")
	fi
done
for f in ${DECODE_FILES[@]+"${DECODE_FILES[@]}"}; do
	# One alternation: grep -P takes a single pattern.
	lit_pattern="atob\s*\(\s*[\"\x27]\K[A-Za-z0-9+/=]{${MIN_B64_LITERAL},}(?=[\"\x27])"
	lit_pattern="$lit_pattern|Buffer\s*\.\s*from\s*\(\s*[\"\x27]\K[A-Za-z0-9+/=]{${MIN_B64_LITERAL},}(?=[\"\x27]\s*,\s*[\"\x27]base64)"
	if [[ $f =~ $CONFIG_RE ]]; then
		lit_pattern="$lit_pattern|[\"\x27\x60]\K[A-Za-z0-9+/=]{32,}(?=[\"\x27\x60])"
	fi
	while IFS= read -r lit; do
		[ -n "$lit" ] || continue
		pad=$(((4 - ${#lit} % 4) % 4))
		[ "$pad" -eq 3 ] && continue # not a valid base64 length
		dec="$(printf '%s%s' "$lit" "$(printf '%*s' "$pad" '' | tr ' ' '=')" |
			base64 -d 2>/dev/null | head -c "$MAX_DECODE_BYTES" | tr -d '\0' || true)"
		[ "${#dec}" -ge 8 ] || continue
		# Mostly-printable only: a decoded binary blob is not evidence.
		nonprint="$(printf '%s' "$dec" | tr -d '[:print:][:space:]' | wc -c)"
		[ "$((nonprint * 10))" -le "${#dec}" ] || continue
		hit="$(printf '%s' "$dec" | grep -oP "$DECODED_INTERESTING" | head -1 || true)"
		[ -n "$hit" ] || continue
		lineno="$(grep -nIF -m1 -e "$lit" -- "$f" 2>/dev/null | cut -d: -f1)"
		emit R12-decoded-payload "$f" "${lineno:-1}" \
			"base64 literal decodes to a C2/marker ('$hit'): ${dec:0:100}"
	done < <(grep -ohIP -e "$lit_pattern" -- "$f" 2>/dev/null | sort -u || true)
done

# Rule 13 — browser-extension manifest tampering. The fake-Phantom extension
# stashed its victim fingerprint in manifest.commands.__meta, which Chrome
# ignores and the payload reads back at runtime. Match the __meta CONTAINER, not
# client_id alone: Petra and Nightly legitimately carry oauth2.client_id.
for f in ${MANIFEST_FILES[@]+"${MANIFEST_FILES[@]}"}; do
	if grep -qzP '"__meta"\s*:\s*\{[^{}]*"client_id"' -- "$f" 2>/dev/null; then
		emit R13-manifest-meta "$f" 1 \
			"extension manifest carries a __meta object with a client_id - attacker-written victim fingerprint"
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
