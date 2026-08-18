# Injected-code scanner

`scripts/scan-injected-code.sh` detects the repo-injection worm (DPRK
"Contagious Interview" / PolinRider class) that infected three contractor
machines and appended an obfuscated dropper to build-config files in every repo
they could write to, recurring every few weeks from 2025-11 to 2026-05 across
`doujins`, `hentai0`, and `doujins-legacy` (`vite.config.js`,
`webpack.config.js`, `frontend/tailwind.config.ts`, `frontend/postcss.config.cjs`).

The payload is appended to the **end of the last line** of a config, behind a
run of ~2784 spaces, so it is invisible in an editor and in `git diff` unless
you scroll right. It then resolves its C2 from a blockchain dead-drop
(trongrid / aptos / bsc RPC), XOR-decodes and `eval`s a next stage, and
re-spawns itself with a detached `child_process`.

## Install the pre-commit hook

Once per clone:

```bash
./scripts/install-git-hooks.sh
```

That symlinks `.git/hooks/pre-commit` to the tracked `scripts/hooks/pre-commit`,
which scans the staged files before every commit. `git commit --no-verify`
bypasses it — only do that after reading the finding.

## Run it by hand

```bash
./scripts/scan-injected-code.sh                       # whole tracked tree (full audit)
./scripts/scan-injected-code.sh --staged              # staged files
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
| `R2-eval-decode` | `eval(` on the same line as `atob(` or `Buffer.from(..., 'base64')` | Decode-and-execute. |
| `R3-detached-spawn` | `detached: true` in a file that also uses `child_process` | The persistence primitive: it re-spawns itself outside the build that started it. |
| `R4-dead-drop` | `trongrid.io`, `aptoslabs.com`, `bsc-dataseed`, `bsc-rpc.publicnode.com` or `eth_getTransactionByHash` **within 1000 characters of** an `eval(`/`atob(`/`child_process`/`detached`/`Buffer.from(...base64)` | Blockchain dead-drop C2 wired to an execution primitive. |
| `R5-marker` | `_$_<4 hex>`, `global['<name>'] = 'N-N-N'`, `global.<name> = 'N-N-N'` | Known obfuscator markers. Fast path only — the version strings and table names change between variants, so this is never the net. |
| `R6-config-line` | A build config (`*.config.{js,cjs,mjs,ts}`, `vite`/`webpack`/`tailwind`/`postcss`/`next.config.*`) with a line over 500 chars | Code appended past the last legitimate statement. The longest legitimate config line in these repos is 97 chars. |

Rules 1 and 6 are structural and catch a payload whose strings you have never
seen; rules 2-5 are behavioural and describe what the samples actually did. The
real 2026-02 sample trips 1, 5 and 6 — its `eval`, `child_process` and C2 hosts
are all inside the encoded string table, which is exactly why the structural
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

Binary files are skipped by every rule (`grep -I`). Without that, raw JPEG bytes
trip R1: two images in `hentai0-legacy` (`public/img/samplenft.jpg`,
`public/common/img/default.jpg`) contain 100+ consecutive space bytes.

### Exclusions

Deliberately minimal — every one below was confirmed necessary against both
working trees:

- `*.md` / `*.mdx` are exempt from **R1 only**. Padded Markdown tables
  legitimately contain long space runs (`docs/frontend-browser-storage.md` has a
  166-space run). Markdown is still subject to every other rule.
- `scripts/testdata/injection-scan/` is exempt from all rules: the fixtures
  carry live signatures on purpose.
- `scripts/scan-injected-code.sh` and this document are exempt from R2-R5 (both
  quote the signatures they describe) but are still subject to R1.

Lockfiles, `*.min.js`, `*.map`, `public/js/`, `dist/` and other vendored or
built directories are **not** excluded — they were checked across all four
repos and produce no blocking findings, so excluding them would only carve out
the kind of unreadable build artifact a payload most wants to live in.
`git ls-files` already keeps `node_modules/` out of scope.

Validated clean at time of writing: `doujins`, `hentai0`,
`doujins-legacy@516a8b7c` (1495 files, 3 notes, 0 findings) and
`hentai0-legacy@881ca98b` (939 files, 0 findings).

## If it fires

Do **not** run, build, or import the flagged file. Inspect it with `cat -A` or
`git diff` and scroll right. If the hit is real, the machine that produced the
commit is compromised: rebuild it, rotate every credential it held, and check
the other repos that machine could write to.

## Caveats

- `--staged` scans the working-tree version of each staged path, not the exact
  blob in the index. The realistic attack (the worm rewrites the file on disk,
  the developer commits it) is covered; a payload staged and then reverted on
  disk in the same commit is not.
- The scanner exempts itself from the content rules, so it is a file worth
  reading in a diff. It is short on purpose.
