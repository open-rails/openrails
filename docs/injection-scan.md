# Injected-code scanner

`scripts/scan-injected-code.sh` detects the repo-injection worm (DPRK
"Contagious Interview" / PolinRider class) that infected three contractor
machines and appended an obfuscated dropper to build-config files in every repo
they could write to, recurring every few weeks from 2025-11 to 2026-05 across
`doujins`, `hentai0`, and `doujins-legacy` (`vite.config.js`,
`webpack.config.js`, `frontend/tailwind.config.ts`, `frontend/postcss.config.cjs`,
and `frontend/scripts/setup-husky.mjs` in `cozy-art`).

The payload is appended to the **end of the last line** of a config, behind a
run of ~2784 spaces, so it is invisible in an editor and in `git diff` unless
you scroll right. It then resolves its C2 from a blockchain dead-drop
(trongrid / aptos / bsc RPC), XOR-decodes and `eval`s a next stage, and
re-spawns itself with a detached `child_process`.

A 184-repo sweep in 2026-08 found the same dropper in **ordinary application
source**, not only in build configs: `bench/surrealdb-ws-bench/bench.ts`
(appended after `main();`) and `source/index.js` (appended after
`handleErrors(main)();`). One of those is *base64-encapsulated* —
`eval("global.o='5-3-160-du';"+atob('…'))` — so its `_$_bb1a` obfuscator table
exists only inside the encoded blob and no plaintext marker scan can see it.
Rule 14 exists because every config-scoped rule is blind to that class, and
rules 2, 5 and 12 are written to survive the encapsulation.

The same actor class also ships browser-extension stealers. A sideloaded fake
Phantom wallet found on a dev machine (2025-12) exfiltrated the wallet password
and the full vault on every unlock, and resolved its C2 from an Aptos dead-drop
account whose URL existed **only base64-encoded**, decoded at runtime with
`atob(...)`. Rules 12 and 13 exist because of that sample.

## Install the pre-commit hook

Once per clone:

```bash
./scripts/install-git-hooks.sh
```

That symlinks `.git/hooks/pre-commit` to the tracked `scripts/hooks/pre-commit`,
which scans the staged blobs before every commit. `git commit --no-verify`
bypasses it — only do that after reading the finding.

## Run it by hand

```bash
./scripts/scan-injected-code.sh                       # whole tracked tree (full audit)
./scripts/scan-injected-code.sh --all --include-untracked
./scripts/scan-injected-code.sh --staged              # staged blobs
./scripts/scan-injected-code.sh --range origin/master...HEAD
./scripts/scan-injected-code.sh --files path/to/file  # named paths, no exclusions
./scripts/scan-injected-code.sh --self-test           # detector's own fixtures
```

Exit code is non-zero if anything is found. CI runs the whole-tree scan and the
self-test on every push and pull request.

## Rules

| Rule | Fires on | Why |
| --- | --- | --- |
| `R1-whitespace` | >=100 consecutive spaces or >=20 tabs, in a non-Markdown file | The padding that hides the payload. Variant-proof — every observed sample used it. |
| `R2-eval-decode` | `eval(` anywhere on the same line as `atob(` or `Buffer.from(..., 'base64')` | Decode-and-execute. Not adjacency: `eval("global.o='…';"+atob('…'))` puts a concatenation between the two and is caught exactly like `eval(atob(…))`. |
| `R3-detached-spawn` | `detached: true` in a file that also uses `child_process` | The persistence primitive: it re-spawns itself outside the build that started it. |
| `R4-dead-drop` | `trongrid.io`, `aptoslabs.com`, `bsc-dataseed`, `bsc-rpc.publicnode.com` or `eth_getTransactionByHash` **within 1000 characters of** an `eval(`/`atob(`/`child_process`/`detached`/`Buffer.from(...base64)` | Blockchain dead-drop C2 wired to an execution primitive. |
| `R5-marker` | `_$_<4 hex>`, or `global.<any name>` / `global['<any name>']` assigned `'N-N-N'` with any number of further `-<alnum>` segments (`'5-3-160'`, `'5-3-160-du'`, `'5-3-361-du'`) | Known obfuscator markers. The key name is a wildcard and the version suffix is open-ended on purpose: `global.i`, `global['_V']` and `global.o` have all been seen, and `-du` appeared in 2026-08. For the base64-encapsulated variant this line is the *only* cleartext leak. |
| `R6-config-line` | A build config with a line over 250 chars | Code appended past the last legitimate statement. |
| `R7-config-interior-ws` | A build config line carrying >=40 whitespace chars **after** its first non-blank | Padding-shape independent: chunked padding (`99 spaces + /**/` repeated) and mixed space/tab runs slip under every run-length limit, but not under a total. |
| `R8-config-unicode-ws` | **Any** U+00A0, U+1680, U+2000-U+200B, U+202F, U+205F, U+2060, U+3000 or U+FEFF in a build config | 400 no-break spaces pad exactly like 400 spaces and are invisible to an ASCII space-run rule. A build config has no reason to contain one. |
| `R8-unicode-ws-run` | A **run** of >=20 of those characters, or >=20 `\r`, in any non-Markdown file | Same padding, outside a config. Run-based on purpose — see below. |
| `R9-config-size` | A build config over 8192 bytes | A hand-written config is small; a dropper is not. Tunable (`MAX_CONFIG_BYTES`) — raise it only for a config you have read end to end. |
| `R10-config-string` | A string literal over 200 chars in a build config | The encoded payload table, including when it is wrapped across short lines so no single line trips R6. |
| `R11-nul-byte` | A NUL byte in a `.js .jsx .ts .tsx .mjs .cjs .go .php .py .sh` file | Not just an oddity: `grep -I` calls such a file binary and skips it, which silently disables R1-R5, R8 and R12 for it. Scoped to source extensions — unscoped it fires on dozens of legitimate binaries. |
| `R12-decoded-payload` | A base64 literal passed to `atob(...)` / `Buffer.from(..., 'base64')` — or, in a build config, **any** base64-shaped literal >=32 chars — that decodes to a dead-drop host, a `0x` chain address, an `http(s)://` URL, or an R5 marker | Decode, *then* match. R4 and R5 read plaintext; the fake-Phantom stealer kept its whole C2 URL inside one `atob("…")` and scored clean. |
| `R13-manifest-meta` | A `manifest.json` whose `"__meta"` object contains a `"client_id"` key | Attacker-written victim fingerprint (`client_id`, `client_os`, `ext_id`, `installed_time`) stashed in a manifest field Chrome ignores and the payload reads back at runtime. |
| `R14-trailing-append` | A whitespace gap of >=40 chars **between two non-blank characters on a file's last non-blank line**, in any non-Markdown file | Code appended past the terminal statement. The only rule that sees a dropper landing in ordinary application source rather than a build config. |

Rules 1, 6, 7, 8, 9, 10 and 14 are structural and catch a payload whose strings
you have never seen; rules 2-5 and 12-13 are behavioural and describe what the
samples actually did. Measured against the real samples:

| Sample | Trips |
| --- | --- |
| 2026-02 config dropper (`webpack.config.js`) | 1, 5, 6, 7, 10, 14 |
| 2026-08 classic variant (`postcss.config.mjs`, `_$_46e0`) | 1, 5, 6, 7, 10, 14 |
| 2026-08 base64-encapsulated, **non-config** (`bench/surrealdb-ws-bench/bench.ts`) | 1, 2, 5, 12, 14 |
| 2026-08 plaintext variant, **non-config** (`source/index.js`) | 1, 5, 14 |
| 2025-12 fake-Phantom extension | 12, 13 |

The config samples never trip 3 or 4: their `eval`, `child_process` and C2 hosts
all live inside the encoded string table, which is exactly why the structural
rules carry the weight.

### Why R4 needs corroboration

A bare hostname is not enough. `doujins-legacy` legitimately ships the Blocto
multi-chain wallet SDK, and three committed bundles (`public/js/746.js`,
`public/js/ios/746.js`, `public/js/ios/node_modules_blocto_sdk_*.js`) name
`bsc-dataseed`. Same-*line* co-occurrence is not enough either: those bundles
are a single 379 KB line, so every string in the SDK shares a line with every
other string. Distance is what separates them — the nearest execution-ish token
in the benign bundles (`detached`) sits 61,100 characters from the hostname,
while a dropper packs its C2 host and its `eval`/`spawn` into one compact blob.

A hostname with nothing near it is still printed, as a non-blocking

```
NOTE path:line: [R4-uncorroborated] ...
```

so a vendored web3 bundle is visible and gets verified once, without failing
the build. Exit status is unaffected by notes.

### Why the scoping is what it is

Every threshold below was measured across all seven trees carrying this scanner
(`doujins`, `hentai0`, `doujins-legacy`, `hentai0-legacy`, `cozy-art`,
`openrails`, `openrails-saas`), and every one produces zero findings on them.

- **R6 at 250, not 500.** The longest legitimate config line across the seven
  trees is 199 (`cozy-art frontend/vite.config.ts:50`).
- **R7 at 40.** The highest legitimate interior-whitespace total is 31.
- **R8 is scoped two ways on purpose.** "Any occurrence" tree-wide costs 34
  false positives — real no-break spaces inside content strings in minified
  CSS/JS. Inside a build config there are none, so a config gets the strict
  form and everything else gets the run form.
- **R9 at 8192 bytes.** The largest legitimate config is 5114 bytes.
- **R10 at 200 chars.** The longest legitimate config string literal is 96.
- **R11 is scoped to source extensions.** Unscoped it fires on every legitimate
  binary in the tree.
- **R12 requires the decoded text to contain something.** "Decodes cleanly" is
  not enough — legitimate web code base64s plenty of harmless data. Literals
  must be base64-charset, >=16 chars, and decode to mostly-printable text.
  Candidate count across the seven trees: zero.
- **R13 matches the `__meta` container, not `client_id`.** The Petra and
  Nightly wallet manifests legitimately carry `oauth2.client_id`; matching
  `client_id` alone turns one true positive into three findings.
- **The config file set does not include a bare `*rc.js`.** It would match the
  vendored `public/vendor/codemirror/mode/mirc/mirc.js` in both legacy trees.
- **R14 is anchored to the last line, and that is what makes it free.** The same
  gap test applied to *every* line costs 9 / 1 / 15 / 11 / 1 / 3 / 2 false
  positives across the seven trees (aligned trailing comments in Go and PHP);
  extending it to the last *three* non-blank lines costs 3; and a
  total-whitespace variant instead of a gap costs up to 158 (minified bundle
  last lines). Anchored to the final non-blank line with a >=40 gap: **0**.

Binary files are skipped by every rule except R11 (`grep -I`). Without that, raw
JPEG bytes trip R1: two images in `hentai0-legacy` (`public/img/samplenft.jpg`,
`public/common/img/default.jpg`) contain 100+ consecutive space bytes.

### Build configs

R6-R10 and the wide form of R12 apply to the build-config set:

- `*.config.<ext>` (any extension), plus
  `vite|webpack|tailwind|postcss|next|rollup|svelte|astro|nuxt.config.*`
- `gulpfile.*`, `Gruntfile.*`
- `setup-*.{js,mjs,cjs,ts}` — the `cozy-art` worm target
  (`frontend/scripts/setup-husky.mjs`) matches no `*.config.*` pattern
- `.husky/*`

### Exclusions

Deliberately minimal — every one below was confirmed necessary against all
seven trees:

- `*.md` / `*.mdx` are exempt from **R1 and R8-unicode-ws-run**. Padded Markdown
  tables legitimately contain long space runs
  (`docs/frontend-browser-storage.md` has a 166-space run). Markdown is still
  subject to every other rule.
- `scripts/testdata/injection-scan/` is exempt from all rules: the fixtures
  carry live signatures on purpose.
- `scripts/scan-injected-code.sh` and this document are exempt from R2-R5 and
  R12 (both quote the signatures they describe) but are still subject to R1.

Lockfiles, `*.min.js`, `*.map`, `public/js/`, `dist/` and other vendored or
built directories are **not** excluded — they were checked across all seven
repos and produce no blocking findings, so excluding them would only carve out
the kind of unreadable build artifact a payload most wants to live in.
`git ls-files` already keeps `node_modules/` out of scope.

Validated clean at time of writing (0 blocking findings, whole tree):
`doujins` (1951 files), `hentai0` (938), `doujins-legacy` (1500, 3 notes),
`hentai0-legacy` (944), `cozy-art` (557), `openrails` (1865),
`openrails-saas` (265).

## If it fires

Do **not** run, build, or import the flagged file. Inspect it with `cat -A` or
`git diff` and scroll right. If the hit is real, the machine that produced the
commit is compromised: rebuild it, rotate every credential it held, and check
the other repos that machine could write to.

## Caveats

- `--all` lists files with `git ls-files`, so **untracked files are not
  scanned**, and neither is anything under `.git/` — including `.git/hooks/*`,
  where a payload can install itself and run on every commit. Pass
  `--all --include-untracked` to add untracked, non-ignored files; `.git/`
  stays out of scope in every mode, so audit hooks by hand
  (`ls -la .git/hooks/`) after cloning or after any incident.
- `--staged` and `--range` read the **blob**, not the working tree: staging a
  payload and then restoring a clean file on disk no longer sails past the
  pre-commit hook. `--range` scans the tip side of the range.
- Rules 12 and 13 are the only ones that look at browser-extension shapes, and
  they only see files that are *in this repo*. An extension sideloaded into
  your browser profile is out of scope — check
  `~/.config/google-chrome/*/Extensions` and Chrome's `Preferences` for
  `from_webstore=false` entries by hand.
- The scanner exempts itself from the content rules, so it is a file worth
  reading in a diff. It is short on purpose.
