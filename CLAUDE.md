# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

- CI runs `go vet ./...` and `go test ./internal/...` — root-package (UI) tests are NOT in CI; run them locally with `go test .` (first run downloads libghostty-vt; `-short` skips those)
- Dev build beside the installed app: `go tool mygo dev` — separate "Kero Dev" app, `kero-dev` data dir, port 7374 (installed app uses 7373)
- Release build: `go tool mygo build -platform "darwin/arm64,..." [-upload]`
- Before changing root-package UI code, read `.agents/skills/mygo-maintenance/SKILL.md` (MyGo Context/element lifecycle, Bind/FocusBind, `ui.NewTester`); `go tool mygo vet .` checks stored build values and goroutine captures
- Website (bun workspace at repo root): `bun run dev` (port 5180) / `bun run build`; deploy from `website/` with `wrangler deploy`

## Testing

- Tests isolate the user environment with `t.Setenv("KERO_HOME", t.TempDir())` plus a temp `KERO_CONFIG` — follow this pattern (see `view_test.go`)

## Style

- Comments and doc comments are English prose explaining why, matching existing files — keep English
- One feature per file in the root package: `xxx.go` + `xxx_test.go`

## Generated files

- `locales/catalog_gen.go` comes from `mygo generate` — never hand-edit; change the source catalogs instead

## Architecture notes (why, not what)

- Single binary, three identities: GUI, `kero` CLI, `kero __kerod` daemon. The daemon owns all PTY sessions; clients auto-spawn it — edits under `internal/` shift the kerod build fingerprint and force daemon restarts
- `proto.Version` bumps only on incompatible protocol changes; adding optional fields does not bump it (see RELEASING.md)
- Dev and installed builds coexist by design (different app name, data dir, port)

## Git

- Commits: English imperative, capitalized first word ("Take a client's reset connection as its close…"); no conventional-commit prefixes, no emoji. Small fixes land directly on main; larger features use a `claude/*` branch with a squash-merged PR
- Cutting a release: follow the **Fork release runbook** below — RELEASING.md's tag-triggers-CI flow does NOT apply to this fork (repo Actions are disabled); every release is built locally

## Fork release runbook (every release, do not skip steps)

The fork's releases are built **entirely on the local machine** — GitHub Actions on QianLongGit/kero are disabled, and its CI lacks the signing/Apple secrets anyway. Prerequisites each release depends on:

1. **The repo stays PUBLIC** (remote installs curl the assets anonymously), and the release must carry the **full server asset set** — a missing asset is a remote-install 404.
2. Keys: the updater's Ed25519 pair lives at `~/Library/Application Support/mygo/update-keys/` (private `mygo-update.key`, never committed). Its public key is mirrored in **two places** that must change together: `mygo.json` `updates.publicKey` and `internal/update/update.go`'s `const PublicKey` (the sign tool checks the latter).

Steps (version X.Y.Z throughout):

1. Bump `mygo.json` `version`, add the user-facing section atop `CHANGELOG.md`, commit `Release X.Y.Z`, `git tag vX.Y.Z`, push `main` and the tag.
2. Build the app assets (no cgo, cross-compiles fine; `-sign - -skip-notarize` = ad-hoc, this fork has no Developer ID):
   `go tool mygo build -platform "darwin/arm64,linux/amd64,linux/arm64" -sign - -skip-notarize`
   → `build/darwin-arm64/kero-X.Y.Z-arm64.dmg`, `build/linux-*/…tar.gz` + `.deb` (Windows installers are CI-only; the server's Windows zips below still cover remote installs).
3. Build the server assets: `sh scripts/build-server.sh` → `build/server/kero-server-X.Y.Z-{os}-{arch}.tar.gz|.zip` + `install-server.sh|.ps1`. The script needs network for libghostty-vt; on `curl: (35)`/SSL errors just rerun it (the route to GitHub is flaky here).
4. Sidecars the script does not write: the sign tool writes a `.sig` and a `.sha256` per archive in one run (`MYGO_UPDATER_PRIVATE_KEY=$(cat ~/Library/Application\ Support/mygo/update-keys/mygo-update.key) go run ./internal/update/sign build/server/kero-server-X.Y.Z-*.tar.gz …zip`); rerun it whenever the script rebuilt an archive — a stale `.sig` fails the updater's Verify. Version-stamp the install scripts if the script left them at the old version (`sed 's/OLD/NEW/g'`).
5. Publish: `gh release create vX.Y.Z -R QianLongGit/kero --draft --title "Kero X.Y.Z" --notes "<CHANGELOG section + ad-hoc signing note>"`, then `gh release upload vX.Y.Z <all assets, globs with * take the .sha256/.sig> -R QianLongGit/kero`, then `gh release edit vX.Y.Z -R QianLongGit/kero --draft=false`. Verify assets ≈ 25.

## Working rules (avoid LLM coding pitfalls)

- Every changed line must trace directly to the request — no drive-by reformatting, comment edits, or refactors of code you were not asked to touch
- Write the minimum code that solves the problem: no speculative abstractions, configurability, or error handling for cases that cannot happen; single-use code gets no abstraction; if 200 lines could be 50, rewrite
- Remove imports/variables your change made unused; leave pre-existing dead code alone unless asked
- "Fix the bug" means write a failing test that reproduces it, then make it pass; "add X" means define acceptance as a test
- If a simpler approach exists, say so and push back when warranted; if a request admits multiple readings, surface them instead of picking one silently
- Trivial changes (typo, one-liner) skip the ceremony
