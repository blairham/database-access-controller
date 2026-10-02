# CLAUDE.md

@AGENTS.md

<!-- AGENTS.md is the cross-tool source of truth; durable project context goes there, not here. -->

## Claude Code-specific notes

- **Slash commands:** `/check` — build + vet + gofumpt + race tests (lint is the commit hook's job).
- Workflow (worktrees, branches, commits) follows `~/Developer/github.com/blairham/AGENTS.md`.
- Run `make generate` after any change under `apis/` — the build fails without
  the deepcopy functions, and the CRD manifests drift silently.
- Integration tests need Docker: `make test-integration` starts and reuses a
  `pgtest` container on port 5433. `make pg-down` stops it — do not leave it running.
