# NPM CLI Distribution Design

**Date:** 2026-09-24

**Status:** Approved design

## Purpose

Make the native `mindctl` CLI available to Node.js users without requiring a
Go toolchain or a separate GitHub Release download. Installing the public
`mindctl` npm package must make the command available through npm's standard
binary linking, including `npx mindctl` and `npm exec mindctl`.

The package is a distribution wrapper for the executable built from
`cmd/mindctl/main.go`; it does not reimplement gateway behavior in JavaScript.

## Scope

Each `v*` router release publishes one npm package named `mindctl`, at the
same version as `cmd/mindctl/VERSION`. The package includes binaries for the
same targets already declared in `.goreleaser.yaml`:

| Node platform | Node architecture | Packaged executable                   |
| ------------- | ----------------- | ------------------------------------- |
| `linux`       | `x64`             | `mindctl` built for linux/amd64       |
| `linux`       | `arm64`           | `mindctl` built for linux/arm64       |
| `darwin`      | `x64`             | `mindctl` built for darwin/amd64      |
| `darwin`      | `arm64`           | `mindctl` built for darwin/arm64      |
| `win32`       | `x64`             | `mindctl.exe` built for windows/amd64 |

The package must ship only the executable payloads required for those targets,
its JavaScript launcher, package metadata, license, and user-facing README.
It must not contain a runtime configuration file, database, secret, or
credential.

## Architecture

`packages/npm` is a standalone, publishable Node.js package. Its `bin` field
maps the `mindctl` command to a portable Node.js launcher. At invocation time,
the launcher selects a bundled executable using `process.platform` and
`process.arch`, then replaces itself with that executable while preserving the
user's arguments, current directory, standard streams, environment, and exit
code.

The source package contains no downloaded executable. The existing GoReleaser
job remains the single producer of native cross-compiled assets. A release
staging script consumes its `dist/` archives, extracts exactly the executable
for each supported target into the npm package payload, checks that every
expected target is present, and writes the release version into the staged
package manifest. The tag-only release job publishes that staged directory
with the existing `NPM_TOKEN` secret only after GoReleaser succeeds.

```text
v* tag
  |
  +-- GoReleaser -- native archives -- GitHub Release
  |                         |
  |                         +-- npm staging script -- one npm package
  |                                                       |
  +-- Docker Buildx -- GHCR                                +-- npm publish
```

This deliberately avoids a `postinstall` downloader. Once npm has installed
the package, command execution needs no GitHub network access, archive tools,
or Go installation.

## Components

- `packages/npm/package.json` declares the public `mindctl` package, a Node
  engine floor compatible with the launcher's APIs, a `bin.mindctl` entry, and
  a restrictive `files` allowlist.
- `packages/npm/bin/mindctl.js` maps Node platform and architecture names to
  the packaged executable path, invokes it synchronously, and propagates its
  termination status. It reports a concise error listing the supported target
  pairs if the host is not supported.
- `packages/npm/scripts/stage.mjs` accepts the GoReleaser output directory
  and release version, extracts the five known archive formats into an
  isolated staging directory, verifies the expected executable names, marks
  POSIX executables as runnable, and creates a publish-ready manifest.
- Package tests test selection and error behavior without executing a native
  router, and test staging against small fixture archives for all five
  supported targets.
- `.github/workflows/release.yml` runs the package tests during the existing
  pull-request router-package job. Its tag-only router-release job stages and
  publishes npm only after the GoReleaser archive step has succeeded.
- `README.md` adds npm installation and execution examples alongside the
  existing native, container, Homebrew, and Go installation options.

## Runtime behavior and errors

For a supported host, the launcher must be transparent: `mindctl version`,
`mindctl --config path`, and every other argument vector reach the compiled
Go executable unchanged. Environment variables such as `XDG_CONFIG_HOME` and
`MINDCTL_DEBUG` remain available to the executable unchanged.

For unsupported operating systems or architectures, the launcher exits
non-zero before spawning anything. Its error states the detected Node target
and enumerates the supported pairs. If a packaged executable is unexpectedly
missing or cannot be launched, it exits non-zero with the resolved executable
path and underlying error; it must not attempt a network download or silently
fall back to another platform binary.

## Release behavior

The source `packages/npm/package.json` carries development metadata only; the
published manifest is generated in staging and uses the normalized tag version
from `cmd/mindctl/VERSION`. Publication runs only on a pushed `v*` tag in the
protected router-release job, using the repository's existing npm token. Pull
requests and local checks may stage and pack the package but must not publish.

The existing GitHub Release archives and OCI image continue unchanged. The npm
package is an additional consumer of GoReleaser output, not another Go build
path, so version, commit, and build-date information continue to be injected
by the established GoReleaser linker flags.

## Verification

- Unit tests cover target-to-path selection for each supported pair, argument
  forwarding, executable exit-code propagation, unsupported targets, and a
  missing packaged executable.
- Staging tests use fixtures to prove each archive maps to the correct npm
  payload path and that a missing or unexpected archive fails clearly.
- `npm pack --dry-run` (or its pnpm equivalent) confirms the package contains
  only the declared publish files and that npm recognizes the `mindctl` bin.
- The pull-request release workflow runs Go tests, GoReleaser snapshot output,
  npm staging, package tests, and a non-publishing package pack.
- The tag workflow publishes only after successful native archive creation.
  A release smoke test installs the packed tarball into a clean temporary npm
  project and confirms `npx mindctl version` invokes the staged executable.

## Non-goals

- Per-platform npm packages or optional dependency packages.
- Downloading GitHub Release artifacts at install time or command invocation.
- Linux architectures beyond amd64 and arm64, Windows arm64, or any target not
  already built by GoReleaser.
- Changing `cmd/mindctl` command semantics, runtime configuration, GoReleaser
  target definitions, or existing GitHub Release and OCI publication behavior.
