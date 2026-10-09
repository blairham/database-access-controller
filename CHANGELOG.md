# Changelog

All notable changes to database-access-controller are recorded here. The
release workflow publishes a tag's section as that GitHub release's notes, and
fails a release whose section is missing: before tagging, move `[Unreleased]`
under `## [X.Y.Z] - <date>`.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
The sections for v0.0.7 and earlier were reconstructed from those releases'
generated notes when this file was started. v0.0.1 to v0.0.6 were published as
`database-controller`; install v0.0.7 or later from
`ghcr.io/blairham/database-access-controller`.

## [Unreleased]

### Security

- Built with Go 1.26.9 and golang.org/x/net v0.60.0, which fix
  GO-2026-6603 to GO-2026-6617 in HTTP/2 and TLS. (#41)

### Changed

- CI and release run blairham/.github's shared workflows. Release signatures
  and provenance now carry the shared workflow's identity
  (`blairham/.github/.github/workflows/go-release.yml`); `SECURITY.md` shows
  the new verify commands, and how to verify v0.0.7 and earlier. (#41)
- Release notes come from this file instead of a generated commit list. (#41)

### Fixed

- The GitHub release is created without naming a commit, so a release whose
  range touches `.github/workflows` no longer fails with a 403. (#39)

## [0.0.7] - 2026-10-08

### Changed

- Renamed from `database-controller` to `database-access-controller`: the
  module, image, chart and binaries all carry the new name. (#36)
- The reconcile loop is built on
  [k8s-controller-kit](https://github.com/blairham/k8s-controller-kit). (#28)
- The Helm chart is published to `oci://ghcr.io/blairham/charts` as a signed
  OCI artifact. (#29)

### Added

- Single hyphens are allowed in role and database names. (#32)
- A warning when two resources share a role and database under
  `revokeOnDelete`. (#34)

### Fixed

- Any catalog relation name is accepted under `ownerOf`. (#33)

## [0.0.6] - 2026-10-05

### Fixed

- Finalizers are patched from a fresh read, so a conflict cannot re-run the
  revoke. (#25)

## [0.0.5] - 2026-10-04

### Added

- Per-resource Ready, pending and freshness metrics, with optional alerts.
  (#19)

## [0.0.4] - 2026-10-04

### Fixed

- The controller acts as `ownerOf` roles the admin creates, and stops
  re-granting owners. (#17)

## [0.0.3] - 2026-10-03

### Added

- Observe mode; the plan is a diff against the database's current state.
  (#15)
- SLSA provenance for the container image, attested in the release job
  rather than through slsa-github-generator. (#11, #14)

## [0.0.2] - 2026-10-03

### Security

- **Breaking:** the server certificate is verified by default. (#9)

### Added

- SLSA provenance for the `dbctl` archives. (#8)

## [0.0.1] - 2026-10-02

### Fixed

- `instance.region` is required only on the IAM auth path. (#1)

## [0.0.0] - 2026-10-02

First published image.

[Unreleased]: https://github.com/blairham/database-access-controller/compare/v0.0.7...HEAD
[0.0.7]: https://github.com/blairham/database-access-controller/compare/v0.0.6...v0.0.7
[0.0.6]: https://github.com/blairham/database-access-controller/compare/v0.0.5...v0.0.6
[0.0.5]: https://github.com/blairham/database-access-controller/compare/v0.0.4...v0.0.5
[0.0.4]: https://github.com/blairham/database-access-controller/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/blairham/database-access-controller/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/blairham/database-access-controller/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/blairham/database-access-controller/compare/v0.0.0...v0.0.1
[0.0.0]: https://github.com/blairham/database-access-controller/tree/v0.0.0
