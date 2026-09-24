# NPM CLI Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a `mindctl` npm package that exposes the native Mindctl CLI
through npm's standard `bin` interface on every platform GoReleaser supports.

**Architecture:** A dependency-free ESM launcher resolves a Node platform and
architecture pair to a bundled executable and forwards the invocation without
changing arguments or environment. A release-only staging script transforms
the existing GoReleaser archives into a minimal publish directory; tag CI
packs and publishes that directory only after GoReleaser has successfully
produced its artifacts.

**Tech Stack:** Node.js built-in modules (`node:child_process`, `node:fs`,
`node:path`, `node:test`), npm package metadata, GoReleaser archives, and
GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-24-npm-cli-distribution-design.md`

## Global Constraints

- Publish one public, unscoped npm package named `mindctl` from `packages/npm`.
- Preserve the five existing target pairs: linux/x64, linux/arm64, darwin/x64,
  darwin/arm64, and win32/x64; do not add a new GoReleaser build target.
- The npm package must contain its binaries, launcher, manifest, license, and
  README only; it must contain no configuration, database, secret, or
  credential.
- The package must not download artifacts during installation or command
  execution and must not require Go or archive tooling on a consumer machine.
- The published package version must equal `cmd/mindctl/VERSION`; only pushed
  `v*` tags may publish it.
- Preserve the compiled CLI's arguments, working directory, environment,
  streams, and exit status. Unsupported and missing-binary failures must be
  explicit and non-zero.
- Run project commands through `devenv shell --`.

## Review Focus

- **Apple Silicon and Linux ARM:** Task 1 pins `darwin/arm64` and
  `linux/arm64` to their own paths rather than an x64 fallback.
- **Windows:** Task 1 pins `win32/x64` to `mindctl.exe`; Task 2 pins extraction
  of the `.zip` asset to that same path.
- **Argument and status transparency:** Task 1 tests a multi-argument vector
  and a non-zero child result so the wrapper cannot hide CLI behavior.
- **Missing or corrupt release assets:** Task 2 tests that a missing archive
  or executable fails staging before a package can be published.
- **Accidental publish leakage:** Task 3 keeps `npm publish` out of pull
  requests and proves the staged tarball only contains the package allowlist.

---

## File Structure

| File                             | Responsibility                                                                                                       |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `packages/npm/package.json`      | Public package metadata, npm bin mapping, scripts, engine floor, and publish allowlist.                              |
| `packages/npm/bin/mindctl.js`    | Thin CLI entrypoint that runs the reusable launcher with the real process values.                                    |
| `packages/npm/lib/targets.mjs`   | Single source of truth for Node target names, GoReleaser archive names, executable names, and package payload paths. |
| `packages/npm/lib/launch.mjs`    | Resolve and spawn the selected bundled executable, including user-facing failures.                                   |
| `packages/npm/scripts/stage.mjs` | Build a clean, versioned npm publish directory from GoReleaser archives.                                             |
| `packages/npm/test/*.test.mjs`   | Node built-in unit and staging tests.                                                                                |
| `packages/npm/README.md`         | npm-specific installation and execution guidance that ships in the package.                                          |
| `README.md`                      | Repository installation section including npm commands.                                                              |
| `pnpm-lock.yaml`                 | Workspace importer record for the new dependency-free package.                                                       |
| `.github/workflows/release.yml`  | Non-publishing pull-request pack proof and tag-only npm publication.                                                 |

### Task 1: Create the package manifest and transparent platform launcher

**Files:**

- Create: `packages/npm/package.json`
- Create: `packages/npm/bin/mindctl.js`
- Create: `packages/npm/lib/targets.mjs`
- Create: `packages/npm/lib/launch.mjs`
- Create: `packages/npm/test/launch.test.mjs`

**Interfaces:**

- Produces `targets`, an immutable array of entries with `platform`, `arch`,
  `goos`, `goarch`, `archiveExtension`, `executable`, and `payloadPath` fields.
- Produces `findTarget(platform, arch): Target | undefined` and
  `supportedTargetLabels(): string[]` from `lib/targets.mjs`.
- Produces `launchMindctl(options): number` from `lib/launch.mjs`, where
  `options` accepts `args`, `platform`, `arch`, `packageRoot`, `spawnSync`,
  and `stderr` to make process behavior testable.
- Consumes Node's `process.platform`, `process.arch`, `process.argv.slice(2)`,
  `process.env`, `process.cwd()`, and `spawnSync` in the executable entrypoint.

- [ ] **Step 1: Write failing target and launch tests**

  Create `packages/npm/test/launch.test.mjs` using `node:test` and
  `node:assert/strict`. Exercise every supported target, then use a fake
  `spawnSync` to pin forwarding and non-zero status propagation:

  ```js
  import assert from "node:assert/strict"
  import test from "node:test"

  import { findTarget } from "../lib/targets.mjs"
  import { launchMindctl } from "../lib/launch.mjs"

  test("maps every supported Node target to a distinct payload", () => {
    assert.equal(
      findTarget("linux", "x64").payloadPath,
      "bin/linux-x64/mindctl"
    )
    assert.equal(
      findTarget("linux", "arm64").payloadPath,
      "bin/linux-arm64/mindctl"
    )
    assert.equal(
      findTarget("darwin", "x64").payloadPath,
      "bin/darwin-x64/mindctl"
    )
    assert.equal(
      findTarget("darwin", "arm64").payloadPath,
      "bin/darwin-arm64/mindctl"
    )
    assert.equal(
      findTarget("win32", "x64").payloadPath,
      "bin/win32-x64/mindctl.exe"
    )
  })

  test("forwards arguments and returns the native exit status", () => {
    const calls = []
    const status = launchMindctl({
      args: ["--config", "router.yaml", "version"],
      platform: "linux",
      arch: "arm64",
      packageRoot: "/package",
      stderr: { write() {} },
      spawnSync: (...args) => {
        calls.push(args)
        return { status: 17 }
      }
    })
    assert.equal(status, 17)
    assert.deepEqual(calls[0][1], ["--config", "router.yaml", "version"])
    assert.equal(calls[0][0], "/package/bin/linux-arm64/mindctl")
  })
  ```

  Add cases for an unsupported `freebsd/x64` pair and a fake `ENOENT` spawn
  error. Require a non-zero result and an error that includes the observed
  target and supported target labels.

- [ ] **Step 2: Run the tests to verify they fail**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/launch.test.mjs
  ```

  Expected: FAIL because the package modules do not exist.

- [ ] **Step 3: Implement the manifest, target registry, launcher, and bin entrypoint**

  Add a dependency-free `package.json` with these publish-facing fields:

  ```json
  {
    "name": "mindctl",
    "version": "0.0.0-development",
    "description": "Mindctl native CLI",
    "license": "Apache-2.0",
    "type": "module",
    "engines": { "node": ">=20" },
    "bin": { "mindctl": "bin/mindctl.js" },
    "files": ["bin", "lib", "README.md", "LICENSE"],
    "scripts": { "test": "node --test test/**/*.test.mjs" }
  }
  ```

  Define target records in `lib/targets.mjs` using the GoReleaser filenames
  `mindctl_<version>_<goos>-<goarch>.tar.gz` for POSIX and
  `mindctl_<version>_windows-amd64.zip` for Windows. `findTarget` compares the
  exact Node `platform` and `arch`; it never substitutes another target.

  In `lib/launch.mjs`, find the target, compose the binary path from
  `packageRoot` and `payloadPath`, and invoke:

  ```js
  const result = spawnSync(binaryPath, args, {
    cwd: process.cwd(),
    env: process.env,
    stdio: "inherit"
  })
  ```

  Return `result.status ?? 1`. For no target or `result.error`, write one
  newline-terminated diagnostic to `stderr` and return `1`. The executable
  `bin/mindctl.js` calls `launchMindctl` with `fileURLToPath(new URL("..",
import.meta.url))` as `packageRoot` and assigns its return value to
  `process.exitCode`.

- [ ] **Step 4: Run focused tests and package manifest inspection**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/launch.test.mjs
  devenv shell -- npm pkg get name bin files --prefix packages/npm
  ```

  Expected: all launcher tests pass; npm reports the `mindctl` name and bin
  mapping without installing a dependency.

- [ ] **Step 5: Commit the launcher deliverable**

  ```bash
  git add packages/npm/package.json packages/npm/bin/mindctl.js \
    packages/npm/lib/targets.mjs packages/npm/lib/launch.mjs \
    packages/npm/test/launch.test.mjs
  git commit -m "feat(npm): add native CLI launcher"
  ```

### Task 2: Stage GoReleaser assets into a minimal npm payload

**Files:**

- Create: `packages/npm/scripts/stage.mjs`
- Create: `packages/npm/test/stage.test.mjs`
- Create: `packages/npm/README.md`
- Modify: `pnpm-lock.yaml`

**Interfaces:**

- Consumes the `targets` records from `packages/npm/lib/targets.mjs` and
  GoReleaser's `dist/mindctl_<version>_<goos>-<goarch>.<extension>` assets.
- Produces `stagePackage({ sourceDir, distDir, outDir, version }): void` and a
  command-line entrypoint accepting `--dist`, `--out`, and `--version`.
- Produces a clean `outDir` with versioned `package.json`, `LICENSE`, package
  README, launcher modules, and one executable at each target `payloadPath`.

- [ ] **Step 1: Write failing staging tests with generated archive fixtures**

  Create `packages/npm/test/stage.test.mjs`. In each test, make a temporary
  source directory containing a short `mindctl` file and a `mindctl.exe` file.
  Use `execFileSync("tar", ["-czf", archive, "-C", source, "mindctl"])` for
  POSIX fixtures and `execFileSync("zip", ["-q", archive, "mindctl.exe"],
{ cwd: source })` for the Windows fixture. Name each archive exactly as
  GoReleaser does for version `1.2.3`.

  Call `stagePackage` and assert that all five `payloadPath` files exist, that
  the staged package manifest contains version `1.2.3`, and that it contains
  no `test` or `scripts` directory. Add one test that omits the
  `darwin-arm64` archive and expects an error mentioning that exact archive
  filename.

- [ ] **Step 2: Run the staging tests to verify they fail**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/stage.test.mjs
  ```

  Expected: FAIL because `stagePackage` does not exist.

- [ ] **Step 3: Implement deterministic staging**

  Implement `stagePackage` in `scripts/stage.mjs` with Node built-ins only.
  Validate that `version` is a non-empty semver string and every target archive
  exists before creating `outDir`. Recreate `outDir`, copy only
  `bin/mindctl.js`, `lib/`, `README.md`, and the repository root `LICENSE`,
  then write a staged manifest copied from the source manifest with the
  supplied version and without development-only scripts.

  Extract POSIX executables with `tar -xOf <archive> mindctl`, extract the
  Windows executable with `unzip -p <archive> mindctl.exe`, and write each
  result to the target `payloadPath`. Mark non-Windows payloads mode `0o755`.
  Treat an archive-command failure, an empty extracted executable, or a
  missing expected archive as a thrown error naming the target and asset. Do
  not invoke a network API, `go`, or a GoReleaser build.

  Parse the three required CLI flags explicitly and exit non-zero with usage
  if any is absent. When executed as a module import, do not parse arguments;
  export `stagePackage` for the tests.

- [ ] **Step 4: Run staging, complete package tests, and package dry-run**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/launch.test.mjs packages/npm/test/stage.test.mjs
  devenv shell -- pnpm install --lockfile-only
  devenv shell -- node packages/npm/scripts/stage.mjs \
    --dist /path/to/goreleaser-dist --out /tmp/mindctl-npm-stage --version 1.2.3
  devenv shell -- npm pack --dry-run --prefix /tmp/mindctl-npm-stage
  ```

  Expected: test suites pass; the lockfile adds the `packages/npm` importer;
  the package dry-run lists only `bin/`, `lib/`, `LICENSE`, `README.md`, and
  `package.json`. For local verification, replace `/path/to/goreleaser-dist`
  with a snapshot `dist/` generated in Task 3.

- [ ] **Step 5: Commit the staging deliverable**

  ```bash
  git add packages/npm/scripts/stage.mjs packages/npm/test/stage.test.mjs \
    packages/npm/README.md pnpm-lock.yaml
  git commit -m "feat(npm): stage GoReleaser binaries for publishing"
  ```

### Task 3: Wire non-publishing CI proof, tag publication, and docs

**Files:**

- Modify: `.github/workflows/release.yml`
- Modify: `README.md`
- Modify: `packages/npm/package.json`
- Test: `packages/npm/test/launch.test.mjs`
- Test: `packages/npm/test/stage.test.mjs`

**Interfaces:**

- Consumes the Task 2 command-line contract:
  `node packages/npm/scripts/stage.mjs --dist dist --out <directory> --version <semver>`.
- Consumes `cmd/mindctl/VERSION` and `GITHUB_REF_NAME` as the release version
  source on a pushed `v*` tag.
- Produces a packed-but-unpublished npm tarball on router-package checks and a
  published `mindctl@<tag version>` only from router-release.

- [ ] **Step 1: Write CI and documentation assertions that fail on the current workflow**

  Add assertions to `packages/npm/test/stage.test.mjs` that the staged
  manifest's `bin.mindctl` remains `bin/mindctl.js`, its version is the input
  version, and its file list excludes test fixtures. Add a small Node test that
  reads `.github/workflows/release.yml` and asserts:

  ```js
  assert.match(workflow, /npm pack --dry-run --prefix dist\/npm/)
  assert.match(workflow, /npm publish dist\/npm --access public/)
  assert.match(
    workflow,
    /NODE_AUTH_TOKEN: \$\{\{ secrets\.STORM_BOT_NPM_TOKEN \}\}/
  )
  ```

  The test must also assert that `npm publish` appears after the
  `goreleaser-action` release command within the `router-release` job, not in
  `router-package`.

- [ ] **Step 2: Run the new assertions to verify they fail**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/stage.test.mjs
  ```

  Expected: FAIL because the current workflow neither stages nor publishes the
  npm package.

- [ ] **Step 3: Add workflow stages and npm documentation**

  Extend the pull-request path filter with `packages/npm/**`, `LICENSE`, and
  `pnpm-lock.yaml`. In `router-package`, set up a supported Node LTS runtime,
  run both package test files, retain the existing GoReleaser snapshot, then
  run:

  ```bash
  VERSION=$(tr -d '\\r\\n' < cmd/mindctl/VERSION)
  node packages/npm/scripts/stage.mjs --dist dist --out dist/npm --version "$VERSION"
  npm pack --dry-run --prefix dist/npm
  ```

  In `router-release`, immediately after the non-snapshot GoReleaser release,
  set up npm registry authentication with `actions/setup-node@v6` and
  `registry-url: https://registry.npmjs.org`. Stage with
  `VERSION=${GITHUB_REF_NAME#v}`, reject a mismatch with
  `cmd/mindctl/VERSION`, and publish with:

  ```bash
  npm publish dist/npm --access public
  ```

  Set `NODE_AUTH_TOKEN: ${{ secrets.STORM_BOT_NPM_TOKEN }}` only on that
  publish step. Do not add a publish command to `router-package`.

  Add an `## npm` subsection to the root installation documentation and the
  package README with:

  ```sh
  npm install --global mindctl
  mindctl version
  
  npx mindctl version
  ```

  State that the npm package contains the current native binary targets and
  that runtime configuration is still required for gateway mode.

- [ ] **Step 4: Run focused verification and a snapshot package smoke test**

  Run:

  ```bash
  devenv shell -- node --test packages/npm/test/launch.test.mjs packages/npm/test/stage.test.mjs
  devenv shell -- goreleaser release --snapshot --clean
  VERSION=$(tr -d '\\r\\n' < cmd/mindctl/VERSION) \
    devenv shell -- node packages/npm/scripts/stage.mjs --dist dist --out dist/npm --version "$VERSION"
  devenv shell -- npm pack --dry-run --prefix dist/npm
  devenv shell -- pnpm exec prettier --check \
    packages/npm README.md .github/workflows/release.yml pnpm-lock.yaml
  git diff --check
  ```

  Then pack `dist/npm`, install that tarball in a fresh temporary npm project,
  and run `npx mindctl version`. On the host target, expect the version from
  `cmd/mindctl/VERSION`; do not claim cross-platform execution without the
  corresponding operating system.

- [ ] **Step 5: Commit the release integration and documentation deliverable**

  ```bash
  git add .github/workflows/release.yml README.md packages/npm/package.json \
    packages/npm/test/stage.test.mjs
  git commit -m "feat(npm): publish Mindctl CLI package"
  ```

## Plan Self-Review

- **Spec coverage:** Task 1 owns command selection, forwarding, and explicit
  invocation errors. Task 2 owns all-five-target release-asset staging, clean
  package contents, and versioned manifest generation. Task 3 owns pull
  request proof, protected tag-only publication, documentation, and an
  installed-tarball smoke test.
- **No placeholders:** The plan contains concrete file paths, target names,
  commands, function contracts, error cases, and staging archive commands;
  no unresolved implementation marker remains.
- **Interface consistency:** `targets`, `findTarget`, `launchMindctl`, and
  `stagePackage` have one owner each; Task 2 and Task 3 consume their stated
  target and staging contracts.
- **Review focus coverage:** ARM target mapping and Windows zip staging are
  pinned in Tasks 1 and 2; forwarding and exit status are pinned in Task 1;
  missing assets in Task 2; and publish isolation plus package contents in
  Task 3.
