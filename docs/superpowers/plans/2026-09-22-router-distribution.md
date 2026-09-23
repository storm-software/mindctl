# Router Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish installable `mindctl` native binaries and a public, multi-architecture GHCR image from each router version tag.

**Architecture:** The Go command exposes injected build metadata through a configuration-free `version` command. GoReleaser cross-compiles and packages native releases while Docker Buildx builds the GHCR image from the same source revision. A pull-request packaging workflow proves both artifact paths without credentials; a tag-only workflow creates a draft GitHub Release, pushes GHCR, then publishes the release.

**Tech Stack:** Go 1.26, GoReleaser v2, Docker Buildx, GitHub Actions, GitHub Container Registry.

**Spec:** `docs/superpowers/specs/2026-09-22-router-distribution-design.md`

## Global Constraints

- Publish native archives for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, and windows/amd64.
- Publish SHA-256 checksums in a `checksums.txt` GitHub Release asset.
- Publish only a Linux amd64/arm64 public manifest at `ghcr.io/storm-software/mindctl`.
- The image has version and commit tags; it does not receive a mutable `latest` tag.
- Release artifacts never contain credentials, encryption keys, SQLite contents, or an active configuration file.
- `mindctl version` succeeds without loading configuration or opening SQLite; normal gateway startup retains its existing configuration behavior.
- Only `v*` tag CI can publish GitHub Release assets or GHCR images; all local and pull-request packaging paths are non-publishing.
- Do not modify the existing `.github/workflows/release.yml` generic workspace-release workflow.

## Review Focus

- `mindctl version` must not attempt the default `config.example.yaml` load; test it in a clean temporary directory with no configuration file.
- Injected release metadata must replace development defaults exactly, including an RFC 3339 build date; test the built binary rather than package variables alone.
- The Windows binary must be archived as ZIP while every Unix target is tar.gz; make `goreleaser check` and a snapshot archive inspection fail for any unexpected extension or target.
- A pull request must not be able to obtain `contents: write`, `packages: write`, GHCR login, or a publishing command; inspect the workflow trigger and `if` conditions in review.
- The runtime image must run as a non-root user and must fail normally when its `/etc/mindctl/config.yaml` mount is absent, rather than carrying a hidden usable configuration.

---

### Task 1: Build metadata and configuration-free version command

**Files:**

- Modify: `cmd/mindctl/main.go`
- Modify: `cmd/mindctl/main_test.go`

**Interfaces:**

- Consumes: linker-set `main.version`, `main.commit`, and `main.date` values.
- Produces: `mindctl version`, which writes exactly three content-free lines to standard output: `version=<value>`, `commit=<value>`, and `date=<value>`.
- Preserves: `run(context.Context, []string, io.Writer, io.Writer) error`, where the first writer is standard output and the second is diagnostics.

- [ ] **Step 1: Write failing unit tests for the version command and development defaults**

  In `cmd/mindctl/main_test.go`, update existing `run` calls to pass separate
  stdout and stderr buffers. Add these tests before changing production code:

  ```go
  func TestRunVersionDoesNotRequireConfiguration(t *testing.T) {
      oldVersion, oldCommit, oldDate := version, commit, date
      t.Cleanup(func() { version, commit, date = oldVersion, oldCommit, oldDate })
      version, commit, date = "dev", "none", "unknown"

      var stdout, stderr bytes.Buffer
      err := run(context.Background(), []string{"version"}, &stdout, &stderr)

      if err != nil {
          t.Fatalf("run version: %v", err)
      }
      if got, want := stdout.String(), "version=dev\\ncommit=none\\ndate=unknown\\n"; got != want {
          t.Fatalf("stdout=%q want=%q", got, want)
      }
      if stderr.Len() != 0 {
          t.Fatalf("stderr=%q", stderr.String())
      }
  }

  func TestRunVersionRejectsArguments(t *testing.T) {
      var stdout, stderr bytes.Buffer
      err := run(context.Background(), []string{"version", "extra"}, &stdout, &stderr)
      if err == nil || !strings.Contains(err.Error(), "unexpected arguments for version") {
          t.Fatalf("error=%v", err)
      }
  }
  ```

- [ ] **Step 2: Run the focused command tests to verify the API is not yet present**

  Run: `devenv shell -- go test ./cmd/mindctl -run 'TestRun(Version|RejectsBadFlags)' -v`

  Expected: FAIL because `run` still accepts one writer and interprets
  `version` as an unexpected positional argument.

- [ ] **Step 3: Add build metadata and dispatch the version subcommand before flag parsing**

  In `cmd/mindctl/main.go`, add development-safe variables next to imports and
  direct successful version output to `stdout` before creating the gateway flag
  set:

  ```go
  var (
      version = "dev"
      commit  = "none"
      date    = "unknown"
  )

  func main() {
      ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
      err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
      stop()
      if err != nil {
          fmt.Fprintln(os.Stderr, "mindctl:", err)
          os.Exit(1)
      }
  }

  func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
      if len(args) > 0 && args[0] == "version" {
          if len(args) != 1 {
              return errors.New("unexpected arguments for version")
          }
          _, err := fmt.Fprintf(stdout, "version=%s\\ncommit=%s\\ndate=%s\\n", version, commit, date)
          return err
      }
      // Keep the existing flag parsing and gateway startup below this branch.
  }
  ```

  Update all existing tests and internal call sites to pass stdout then stderr.
  Do not change gateway log routing: startup messages and errors still go to
  stderr.

- [ ] **Step 4: Add a linker-injection integration test**

  Extend `TestBinarySignalsAndContentFreeStartup` with a subtest that builds
  from `cmd/mindctl` using explicit linker flags, runs `version` in an empty
  temporary directory, and asserts exact injected values:

  ```go
  build := exec.Command("go", "build",
      "-ldflags", "-X main.version=1.2.3 -X main.commit=deadbeef -X main.date=2026-09-22T12:00:00Z",
      "-o", binary, ".")
  build.Dir = "."
  if output, err := build.CombinedOutput(); err != nil {
      t.Fatalf("build: %v\\n%s", err, output)
  }
  cmd := exec.Command(binary, "version")
  cmd.Dir = t.TempDir()
  output, err := cmd.Output()
  if err != nil {
      t.Fatalf("version: %v", err)
  }
  if got, want := string(output), "version=1.2.3\\ncommit=deadbeef\\ndate=2026-09-22T12:00:00Z\\n"; got != want {
      t.Fatalf("version output=%q want=%q", got, want)
  }
  ```

- [ ] **Step 5: Run command tests and the full Go suite**

  Run:

  ```bash
  devenv shell -- go test ./cmd/mindctl -v
  devenv shell -- go test ./...
  ```

  Expected: PASS. The version test never requires any environment secret or
  configuration file; existing signal and gateway-start tests still pass.

- [ ] **Step 6: Commit the independently usable command interface**

  ```bash
  git add cmd/mindctl/main.go cmd/mindctl/main_test.go
  git commit -m "feat(router): expose build metadata command"
  ```

### Task 2: Native archive configuration and verification workflow

**Files:**

- Create: `.goreleaser.yaml`
- Create: `.github/workflows/router-package.yml`
- Modify: `.gitignore`

**Interfaces:**

- Consumes: a clean `v*`-compatible Git revision and Go source rooted at
  `./cmd/mindctl`.
- Produces: `dist/mindctl_<version>_<os>_<arch>.tar.gz` for Linux and macOS,
  `dist/mindctl_<version>_windows_amd64.zip`, and `dist/checksums.txt`.
- Produces: a non-publishing pull-request/manual workflow that validates the
  GoReleaser configuration and produces a snapshot archive.

- [ ] **Step 1: Create a failing package-contract check**

  Create `.github/workflows/router-package.yml` with a temporary first step
  that runs `test -f .goreleaser.yaml`. This makes the packaging workflow's
  contract explicit before configuration exists:

  ```yaml
  name: Router package

  on:
    pull_request:
      paths:
        - "cmd/mindctl/**"
        - "internal/**"
        - ".goreleaser.yaml"
        - "Dockerfile"
        - ".github/workflows/router-package.yml"
    workflow_dispatch:

  permissions:
    contents: read

  jobs:
    package:
      runs-on: ubuntu-latest
      steps:
        - run: test -f .goreleaser.yaml
  ```

- [ ] **Step 2: Run the local contract check to verify it fails**

  Run: `test -f .goreleaser.yaml`

  Expected: FAIL because no native packaging configuration exists.

- [ ] **Step 3: Define reproducible native release artifacts**

  Create `.goreleaser.yaml` with the complete free GoReleaser v2 configuration:

  ```yaml
  version: 2
  project_name: mindctl

  builds:
    - id: mindctl
      main: ./cmd/mindctl
      binary: mindctl
      env:
        - CGO_ENABLED=0
      goos: [linux, darwin, windows]
      goarch: [amd64, arm64]
      ignore:
        - goos: windows
          goarch: arm64
      ldflags:
        - -s -w -X main.version={{ .Version }} -X main.commit={{ .Commit }} -X main.date={{ .Date }}

  archives:
    - id: mindctl
      ids: [mindctl]
      name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
      formats: [tar.gz]
      format_overrides:
        - goos: windows
          formats: [zip]

  checksum:
    name_template: checksums.txt
  ```

  Add `dist/` to `.gitignore`. Do not configure Docker publication here;
  Buildx owns the image and GoReleaser owns archives/checksums only.

- [ ] **Step 4: Replace the temporary check with an executable non-publishing package workflow**

  Replace the placeholder job in `.github/workflows/router-package.yml` with
  checkout, Go setup, Go tests, GoReleaser validation, a snapshot archive
  build, and an uploaded `dist` artifact. Pin the permissions to read-only and
  never add a registry login step:

  ```yaml
  jobs:
    package:
      runs-on: ubuntu-latest
      steps:
        - uses: actions/checkout@v5
          with:
            fetch-depth: 0
        - uses: actions/setup-go@v6
          with:
            go-version-file: go.mod
        - run: go test ./...
        - uses: goreleaser/goreleaser-action@v6
          with:
            distribution: goreleaser
            version: "~> v2"
            args: check
        - uses: goreleaser/goreleaser-action@v6
          with:
            distribution: goreleaser
            version: "~> v2"
            args: release --snapshot --clean
        - uses: actions/upload-artifact@v4
          with:
            name: mindctl-snapshot
            path: dist/
  ```

  Include `README.md` in the pull-request path filter so installation-command
  edits receive the same packaging proof.

- [ ] **Step 5: Validate archive formats and checksum coverage locally**

  Run:

  ```bash
  devenv shell -- goreleaser check
  devenv shell -- goreleaser release --snapshot --clean
  find dist -maxdepth 1 -type f -printf '%f\n' | sort
  (cd dist && sha256sum --check checksums.txt)
  ```

  Expected: `goreleaser check` succeeds; `dist` contains five archives plus
  `checksums.txt`; the four Linux/macOS archives end in `.tar.gz`, the Windows
  archive ends in `.zip`, and all checksum lines verify.

- [ ] **Step 6: Commit native distribution support**

  ```bash
  git add .goreleaser.yaml .github/workflows/router-package.yml .gitignore
  git commit -m "build(router): package native release archives"
  ```

### Task 3: Non-root OCI image and pre-publish image verification

**Files:**

- Create: `Dockerfile`
- Create: `.dockerignore`
- Modify: `.github/workflows/router-package.yml`

**Interfaces:**

- Consumes: `VERSION`, `COMMIT`, and `DATE` Docker build arguments, plus an
  optional runtime configuration mounted at `/etc/mindctl/config.yaml`.
- Produces: a Linux router image whose entrypoint is `/usr/local/bin/mindctl`
  and whose default command is `-config /etc/mindctl/config.yaml`.
- Produces: a non-pushing Buildx validation step for linux/amd64 and
  linux/arm64 in the package workflow.

- [ ] **Step 1: Add a failing container contract check**

  Add the following local test command to the `router-package.yml` package job
  immediately before the artifact upload, before adding a Dockerfile:

  ```yaml
        - run: docker buildx build --platform linux/amd64,linux/arm64 --file Dockerfile .
  ```

  Run: `docker buildx build --platform linux/amd64,linux/arm64 --file Dockerfile .`

  Expected: FAIL because `Dockerfile` does not exist.

- [ ] **Step 2: Build the router without a configuration payload**

  Create `Dockerfile` as follows:

  ```dockerfile
  # syntax=docker/dockerfile:1
  FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

  ARG TARGETOS
  ARG TARGETARCH
  ARG VERSION=dev
  ARG COMMIT=none
  ARG DATE=unknown

  WORKDIR /src
  COPY go.mod go.sum ./
  RUN go mod download
  COPY cmd ./cmd
  COPY internal ./internal
  COPY migrations ./migrations
  RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/mindctl ./cmd/mindctl

  FROM gcr.io/distroless/static-debian12:nonroot
  LABEL org.opencontainers.image.title="mindctl" \
        org.opencontainers.image.description="Mindctl AI model router" \
        org.opencontainers.image.licenses="Apache-2.0"
  COPY --from=build /out/mindctl /usr/local/bin/mindctl
  USER nonroot:nonroot
  EXPOSE 8080
  ENTRYPOINT ["/usr/local/bin/mindctl"]
  CMD ["-config", "/etc/mindctl/config.yaml"]
  ```

  Create `.dockerignore` to exclude repository-local data and release output:

  ```text
  .git
  .github
  .nx
  dist
  node_modules
  tmp
  *.db
  .env
  .env.*
  config.example.yaml
  ```

- [ ] **Step 3: Add image validation to the non-publishing workflow**

  Before the Buildx command, add QEMU and Buildx setup. Set `push: false` and
  provide fixed non-secret metadata arguments so this path cannot publish:

  ```yaml
        - uses: docker/setup-qemu-action@v3
        - uses: docker/setup-buildx-action@v3
        - uses: docker/build-push-action@v6
          with:
            context: .
            platforms: linux/amd64,linux/arm64
            push: false
            build-args: |
              VERSION=snapshot
              COMMIT=${{ github.sha }}
              DATE=1970-01-01T00:00:00Z
  ```

  Remove the temporary raw `docker buildx build` workflow command after the
  action is present. The workflow must contain no `docker/login-action`,
  `packages: write`, or `contents: write` declaration.

- [ ] **Step 4: Verify the amd64 image runtime contract locally**

  Run:

  ```bash
  docker build --build-arg VERSION=1.2.3 --build-arg COMMIT=deadbeef --build-arg DATE=2026-09-22T12:00:00Z -t mindctl:test .
  docker run --rm --entrypoint /usr/local/bin/mindctl mindctl:test version
  docker image inspect mindctl:test --format '{{.Config.User}} {{json .Config.Cmd}}'
  docker run --rm mindctl:test
  ```

  Expected: version output matches the injected three lines; inspection reports
  `nonroot:nonroot` and the config command; the default invocation fails
  non-zero because `/etc/mindctl/config.yaml` was not mounted. Its output must
  not contain a usable secret or embedded config.

- [ ] **Step 5: Commit container packaging and validation**

  ```bash
  git add Dockerfile .dockerignore .github/workflows/router-package.yml
  git commit -m "build(router): add non-root container image"
  ```

### Task 4: Tag-only GitHub Release and GHCR publication

**Files:**

- Create: `.github/workflows/router-release.yml`

**Interfaces:**

- Consumes: a pushed Git tag matching `v*` and the repository-provided
  `GITHUB_TOKEN`.
- Produces: a GitHub Release containing GoReleaser archives/checksums and a
  public multi-platform GHCR image tagged with the semver version and short
  commit SHA.
- Preserves: the generic `.github/workflows/release.yml` unchanged.

- [ ] **Step 1: Create a failing tag-gate contract**

  Create `.github/workflows/router-release.yml` with this initial trigger and
  inspect it before adding publishing steps:

  ```yaml
  name: Router release

  on:
    push:
      tags:
        - "v*"
  ```

  Run: `rg -n 'pull_request|branches:|tags:' .github/workflows/router-release.yml`

  Expected: output contains only the `push.tags` release trigger; it must not
  show a pull-request trigger or a branch release trigger.

- [ ] **Step 2: Implement ordered publishing with least-privilege credentials**

  Complete the workflow using the following structure. Keep the release draft
  private until GHCR publication succeeds, so a public GitHub Release never
  claims an image that failed to publish:

  ```yaml
  permissions:
    contents: write
    packages: write

  jobs:
    release:
      runs-on: ubuntu-latest
      steps:
        - uses: actions/checkout@v5
          with:
            fetch-depth: 0
        - uses: actions/setup-go@v6
          with:
            go-version-file: go.mod
        - run: go test ./...
        - id: build
          run: echo "date=$(date --utc +%Y-%m-%dT%H:%M:%SZ)" >> "$GITHUB_OUTPUT"
        - uses: goreleaser/goreleaser-action@v6
          with:
            distribution: goreleaser
            version: "~> v2"
            args: release --clean
          env:
            GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        - uses: docker/setup-qemu-action@v3
        - uses: docker/setup-buildx-action@v3
        - uses: docker/login-action@v3
          with:
            registry: ghcr.io
            username: ${{ github.actor }}
            password: ${{ secrets.GITHUB_TOKEN }}
        - id: metadata
          uses: docker/metadata-action@v5
          with:
            images: ghcr.io/${{ github.repository }}
            tags: |
              type=semver,pattern={{version}}
              type=sha,format=short
        - uses: docker/build-push-action@v6
          with:
            context: .
            platforms: linux/amd64,linux/arm64
            push: true
            tags: ${{ steps.metadata.outputs.tags }}
            labels: ${{ steps.metadata.outputs.labels }}
            build-args: |
              VERSION=${{ steps.metadata.outputs.version }}
              COMMIT=${{ github.sha }}
              DATE=${{ steps.build.outputs.date }}
        - run: gh release edit "${GITHUB_REF_NAME}" --draft=false
          env:
            GH_TOKEN: ${{ github.token }}
  ```

  Add `draft: true` to the `.goreleaser.yaml` `release` stanza so the final
  `gh release edit` is the sole publication step:

  ```yaml
  release:
    draft: true
  ```

- [ ] **Step 3: Verify release configuration without publishing**

  Run:

  ```bash
  devenv shell -- goreleaser check
  git diff --check
  rg -n 'pull_request|branches:|docker/login-action|contents: write|packages: write|push: true' .github/workflows/router-*.yml
  ```

  Expected: GoReleaser validates. Only `router-release.yml` contains write
  permissions, registry login, and `push: true`; `router-package.yml` remains
  read-only and non-publishing. The release workflow is tag-only.

- [ ] **Step 4: Manually inspect the first tag release in GitHub Actions**

  Push a non-production prerelease tag such as `v0.0.1-rc.1` through the normal
  review-and-merge process. Confirm all of the following in GitHub after the
  workflow finishes:

  ```text
  GitHub Release: five archives and checksums.txt are visible, no draft badge remains
  GHCR: ghcr.io/storm-software/mindctl:0.0.1-rc.1 has linux/amd64 and linux/arm64
  GHCR: ghcr.io/storm-software/mindctl:sha-<short-sha> resolves to the same manifest
  ```

  If GHCR fails, leave the generated GitHub Release draft unpublished and use
  the failed workflow logs to correct only the failing configuration before a
  new tag is created.

- [ ] **Step 5: Commit tag publication automation**

  ```bash
  git add .goreleaser.yaml .github/workflows/router-release.yml
  git commit -m "ci(router): publish release artifacts and image"
  ```

### Task 5: Accurate installation and runtime-configuration documentation

**Files:**

- Modify: `README.md`

**Interfaces:**

- Consumes: the native archive names, image location, `mindctl version`
  command, and `/etc/mindctl/config.yaml` image convention from Tasks 1-4.
- Produces: user-facing native, container, and source installation instructions
  that identify required configuration without suggesting an embedded default.

- [ ] **Step 1: Replace stale template installation content with concrete router instructions**

  In `README.md`, replace the existing template-era `Getting Started` through
  `Testing` content (including React/Nx/Cypress/Jest claims) with the following
  focused sections:

  ```markdown
  ## Install

  ### Native binary

  Download the archive for your platform from the GitHub Release, verify it
  against `checksums.txt`, unpack it, and run:

  ```sh
  ./mindctl version
  ./mindctl -config ./config.yaml
  ```

  ### Container

  Pull a versioned image and mount a runtime configuration file:

  ```sh
  docker pull ghcr.io/storm-software/mindctl:0.0.1
  docker run --rm -p 8080:8080 \
    -v "$PWD/config.yaml:/etc/mindctl/config.yaml:ro" \
    --env-file .env \
    ghcr.io/storm-software/mindctl:0.0.1
  ```

  The image runs as a non-root user and contains no usable configuration,
  credentials, encryption keys, or database. Configure a writable SQLite path
  in `config.yaml` when persistence is enabled.

  ### From source

  ```sh
  devenv shell -- go run ./cmd/mindctl -config ./config.yaml
  ```
  ```

  Keep the project overview and contribution/license sections intact. Update
  the table of contents to list `Install` and its three subsections.

- [ ] **Step 2: Add exact checksum verification examples**

  Immediately after the native download instruction, document both supported
  local verification commands:

  ```markdown
  On Linux, run `sha256sum --check checksums.txt`. On macOS, compare
  `shasum -a 256 <archive>` to the matching `checksums.txt` entry.
  ```

  Do not provide a package-manager command because formulae and package
  repositories are explicitly out of scope.

- [ ] **Step 3: Check documentation formatting and command interfaces**

  Run:

  ```bash
  devenv shell -- go build -o /tmp/mindctl ./cmd/mindctl
  /tmp/mindctl version
  devenv shell -- pnpm exec prettier --check README.md .goreleaser.yaml .github/workflows/router-package.yml .github/workflows/router-release.yml Dockerfile .dockerignore
  git diff --check
  ```

  Expected: the built binary prints the documented metadata fields; all edited
  prose and configuration files format cleanly; no whitespace errors occur.

- [ ] **Step 4: Commit release documentation**

  ```bash
  git add README.md
  git commit -m "docs(router): document binary and container installation"
  ```

## Final verification

- [ ] Run `devenv shell -- go test ./...` and require a zero exit code.
- [ ] Run `devenv shell -- goreleaser check` and `devenv shell -- goreleaser release --snapshot --clean`; inspect that five archives and `checksums.txt` are produced and checksum verification succeeds.
- [ ] Run the local amd64 Docker build and `mindctl version` command from Task 3; confirm the image declares a non-root user and fails normally without the required config mount.
- [ ] Inspect `router-package.yml` and `router-release.yml` to confirm only the tag workflow has write permissions, GHCR login, or `push: true`.
- [ ] Run `git status --short` and `git diff --check`; preserve the user's pre-existing `package.json` modification and report it separately.
