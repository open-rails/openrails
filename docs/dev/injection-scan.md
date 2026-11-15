# Injected-code scan

`scripts/scan-injected-code.sh` detects a repository-injection worm that appends
an obfuscated dropper to the end of a file's last line, behind thousands of
spaces, so it is invisible in an editor or a diff. The dropper resolves its C2 from
a blockchain dead-drop, decodes and `eval`s a next stage, and re-spawns detached.
It has appeared in build configs and in ordinary application source, sometimes
base64-encapsulated, and the same actor ships browser-extension stealers.

```bash
./scripts/install-git-hooks.sh                 # once per clone: pre-commit scans staged blobs
./scripts/scan-injected-code.sh                # whole tracked tree
./scripts/scan-injected-code.sh --all --include-untracked
./scripts/scan-injected-code.sh --staged | --range origin/master...HEAD | --files PATH…
./scripts/scan-injected-code.sh --self-test    # the detector's own fixtures
```

It exits non-zero on a finding; CI runs the tree scan and the self-test.
`--staged` and `--range` read blobs, not the working tree.

| Rule | Fires on |
|---|---|
| R1 | 100+ spaces or 20+ tabs in a row, outside Markdown |
| R2 | `eval(` on a line with a base64 decode |
| R3 | `detached: true` in a file using `child_process` |
| R4 | a dead-drop host within 1000 characters of an execution primitive (alone: a non-blocking `NOTE`) |
| R5 | known obfuscator markers |
| R6–R10 | build configs: a line over 250 chars, 40+ interior whitespace, any Unicode space, over 8192 bytes, a string over 200 chars |
| R8 run | 20+ Unicode spaces or `\r` in a row, outside Markdown |
| R11 | a NUL byte in a source file (it would make `grep` skip the file) |
| R12 | a base64 literal that decodes to a dead-drop host, chain address, URL or marker |
| R13 | a `manifest.json` whose `__meta` holds `client_id` |
| R14 | a 40+ whitespace gap between code on a file's last non-blank line |

Build configs are `*.config.*`, `gulpfile.*`, `Gruntfile.*`,
`setup-*.{js,mjs,cjs,ts}` and `.husky/*`. Every threshold was measured to give zero
false positives across the repositories carrying the scanner.

Exclusions are minimal: Markdown is exempt from R1 and the R8 run;
`scripts/testdata/injection-scan/` from everything (its fixtures carry live
signatures); the scanner and this page from the content rules, since they quote
signatures. Vendored and built files are scanned.

**If it fires,** do not run, build or import the file. Read it with `cat -A` or a
diff scrolled right. A real hit means the machine that made the commit is
compromised: rebuild it, rotate its credentials and check every repository it could
write to. Hooks under `.git/hooks/` and sideloaded browser extensions are out of
scope: check them by hand.
