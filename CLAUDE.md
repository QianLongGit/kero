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
- Cutting a release: follow [RELEASING.md](RELEASING.md) — mygo.json version must match the tag; CHANGELOG entries are user-facing

## Working rules (avoid LLM coding pitfalls)

- Every changed line must trace directly to the request — no drive-by reformatting, comment edits, or refactors of code you were not asked to touch
- Write the minimum code that solves the problem: no speculative abstractions, configurability, or error handling for cases that cannot happen; single-use code gets no abstraction; if 200 lines could be 50, rewrite
- Remove imports/variables your change made unused; leave pre-existing dead code alone unless asked
- "Fix the bug" means write a failing test that reproduces it, then make it pass; "add X" means define acceptance as a test
- If a simpler approach exists, say so and push back when warranted; if a request admits multiple readings, surface them instead of picking one silently
- Trivial changes (typo, one-liner) skip the ceremony
