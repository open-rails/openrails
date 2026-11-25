# Open release candidate

`rc/0.284.0` is open for the order → invoice rename, AuthKit 1.55 adoption, admin screens embedded in a merchant's own site (and a console that signs in only with OpenRails accounts or a probed, configured issuer), a seeded local operator for `task dev`, and the CI merge queue running the full suite.

Work lands on the one open release candidate named here, never on `master`: push to it after a local build, and its push check runs the light tier. Pull requests into it are optional. An RC is named for the version it becomes (`rc/0.280.0`). When one is cut from master, this file names it and what it is for; when it merges to master the branch is deleted and this file says none again. See [`.claude/CLAUDE.md`](.claude/CLAUDE.md) "Branches".
