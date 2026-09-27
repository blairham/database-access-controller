# CLAUDE.md

@AGENTS.md

<!-- AGENTS.md is the source of truth for this repo; keep durable context there. -->

## Claude Code-specific notes

- Run `make generate` after any change under `apis/` — the build fails without
  the deepcopy functions, and the CRD manifests drift silently.
- Integration tests need Docker: `make test-integration` starts and reuses a
  `pgtest` container on port 5433. `make pg-down` stops it.
