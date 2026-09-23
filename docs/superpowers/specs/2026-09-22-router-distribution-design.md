# Router Distribution Design

**Date:** 2026-09-22

**Status:** Approved design

## Purpose

Make the `mindctl` router usable without a source checkout by publishing two
artifacts from each version tag:

- GitHub Release archives for supported desktop and server platforms.
- A public multi-architecture OCI image at
  `ghcr.io/storm-software/mindctl`.

The router remains configured entirely at runtime. No configuration files,
database contents, credentials, or encryption keys are included in release
artifacts.

## Scope

The release workflow runs on pushed tags matching `v*`. It publishes:

| Target | Platforms | Artifact |
| --- | --- | --- |
| Native binary | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 | Versioned `.tar.gz` or `.zip` archive |
| Integrity | All native archives | `checksums.txt` containing SHA-256 digests |
| OCI image | linux/amd64, linux/arm64 | `ghcr.io/storm-software/mindctl:<version>`, plus the matching commit tag |

The GitHub Release receives the native archives and checksum manifest. The OCI
registry receives a multi-platform manifest, not separate architecture-specific
tags as the public interface.

## Architecture

GoReleaser owns Go cross-compilation, archive naming, release notes handoff,
and checksum creation. A dedicated GitHub Actions workflow owns release
credentials, GitHub Release publication, and OCI image publication through
Docker Buildx. Separating the image build from archive creation keeps Docker
platform and registry behavior explicit and avoids coupling image publication
to a GoReleaser-specific Docker configuration.

```text
v* tag
  |
  +-- GoReleaser -- Go archives + checksums -- GitHub Release
  |
  +-- Docker Buildx -- linux/amd64 + linux/arm64 -- GHCR manifest
```

Both paths derive their version, commit, and build date from the triggering
tag and GitHub workflow metadata. The binary exports those values with linker
flags and exposes them through `mindctl version` without loading configuration
or opening storage. The image uses the same linker flags and OCI standard
labels.

## Components

- `cmd/mindctl`: add a `version` subcommand and package-scoped build metadata
  variables with development-safe defaults.
- `.goreleaser.yaml`: declare supported binary archives, reproducible naming,
  SHA-256 checksums, and linker flags.
- `Dockerfile`: build a statically linked Linux binary for the Buildx target;
  copy it into a minimal runtime image, run as a non-root user, and expose the
  service port. It does not copy `config.example.yaml` as an active
  configuration.
- `.github/workflows/router-release.yml`: on `v*` tags, grant only contents
  write and packages write permissions, run GoReleaser, and build/push the
  multi-platform GHCR image.
- `README.md`: replace template-era build guidance with binary, container, and
  source-run installation instructions, including mandatory runtime
  configuration.

The pre-existing generic repository release workflow remains untouched: it
continues to handle workspace release automation and has no dependable tag
artifact contract for the Go application.

## Runtime behavior and errors

`mindctl version` accepts no positional arguments and prints only version,
commit, and build date. It exits successfully without requiring a config file.
The default invocation remains the gateway start command and preserves its
current configuration validation and non-zero failures.

The image starts the normal gateway command. Users must mount or otherwise
provide a configuration file and required environment variables. Missing or
invalid configuration fails at startup exactly as the native binary does.

Only tag-triggered CI may push release assets or OCI images. Pull requests and
local dry runs build artifacts but cannot publish them.

## Verification

- Unit tests cover development defaults and `mindctl version` output without a
  configuration file, while retaining existing gateway command tests.
- Configuration validation confirms every declared target and archive format
  is intentional.
- `go test ./...` verifies the application.
- A GoReleaser snapshot/release dry run builds archives and the checksum file
  without publishing.
- A local Docker build validates the Dockerfile; CI's Buildx step validates
  the multi-platform manifest and labels before the protected publish step.
- Documentation commands are checked against the produced binary and image
  interface.

## Non-goals

- Package-manager formulae (Homebrew, Scoop, apt, rpm, or deb).
- Runtime configuration hot reload or an embedded default configuration.
- Image signing, SBOMs, or provenance attestations beyond the registry's
  normal workflow metadata. These can be added as a separately reviewed
  supply-chain hardening effort.
- Altering the existing generic workspace-release action.
