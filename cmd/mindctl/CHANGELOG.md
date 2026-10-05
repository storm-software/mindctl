![Mindctl's logo banner](https://public.storm-cdn.com/mindctl/media/banner-1280x320-dark.gif)

# Changelog for Mindctl - Mindctl

## [0.1.45](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.45) (10/05/2026)

### Bug Fixes

- **mindctl:** Apply fix for unmarshalled JSON types ([81a256f](https://github.com/storm-software/mindctl/commit/81a256f))

## [0.1.42](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.42) (09/29/2026)

### Bug Fixes

- **mindctl:** Resolve issue truncating request/responses ([62b1e20](https://github.com/storm-software/mindctl/commit/62b1e20))

## [0.1.41](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.41) (09/28/2026)

### Features

- **mindctl:** Added harness hooks ([efdef5e](https://github.com/storm-software/mindctl/commit/efdef5e))

## [0.1.40](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.40) (09/27/2026)

### Features

- **mindctl:** Added harness on/off/uninstall commands to the CLI ([0157b2e](https://github.com/storm-software/mindctl/commit/0157b2e))
- **mindctl:** Added the `serve` command and support for specifying harnesses for setup ([980a3e3](https://github.com/storm-software/mindctl/commit/980a3e3))

## [0.1.39](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.39) (09/27/2026)

### Bug Fixes

- **mindctl:** Resolved the content must not be empty error ([0e17ff0](https://github.com/storm-software/mindctl/commit/0e17ff0))

## [0.1.38](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.38) (09/27/2026)

### Features

- **mindctl:** Add Anthropic request cache to router ([542f3ab](https://github.com/storm-software/mindctl/commit/542f3ab))
- **mindctl:** Added savings calculation to router ([dea8d9d](https://github.com/storm-software/mindctl/commit/dea8d9d))

## [0.1.37](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.37) (09/27/2026)

### Bug Fixes

- **mindctl:** Clean up the history table display ([59bce64](https://github.com/storm-software/mindctl/commit/59bce64))

## [0.1.36](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.36) (09/27/2026)

### Bug Fixes

- **mindctl:** Clean up the table display using `go-pretty` package ([e60d9e0](https://github.com/storm-software/mindctl/commit/e60d9e0))

## [0.1.35](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.35) (09/27/2026)

### Bug Fixes

- **mindctl:** Clean up history display and resolve issue with Haiku model ([33ed329](https://github.com/storm-software/mindctl/commit/33ed329))

## [0.1.34](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.34) (09/27/2026)

### Bug Fixes

- **mindctl:** Improve display of history command ([aa14d84](https://github.com/storm-software/mindctl/commit/aa14d84))

## [0.1.33](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.33) (09/27/2026)

### Bug Fixes

- **mindctl:** Ensure we do not cutoff min-word in history truncation ([d5ca8f7](https://github.com/storm-software/mindctl/commit/d5ca8f7))

### Features

- **mindctl:** Store explicit model selection history ([a292e1e](https://github.com/storm-software/mindctl/commit/a292e1e))

## [0.1.32](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.32) (09/27/2026)

### Bug Fixes

- **mindctl:** Resolve API error in Claude Code using Headroom compression ([3fe7f3b](https://github.com/storm-software/mindctl/commit/3fe7f3b))

## [0.1.29](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.29) (09/26/2026)

### Bug Fixes

- **mindctl:** Resolve Anthropic messaging issues ([9aca4e2](https://github.com/storm-software/mindctl/commit/9aca4e2))

## [0.1.27](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.27) (09/26/2026)

### Bug Fixes

- **mindctl:** Resolve issue with large request/response display ([d9a8f73](https://github.com/storm-software/mindctl/commit/d9a8f73))

## [0.1.25](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.25) (09/26/2026)

### Bug Fixes

- **mindctl:** Update `history` to display last 20 by default ([ed91fd0](https://github.com/storm-software/mindctl/commit/ed91fd0))

## [0.1.24](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.24) (09/26/2026)

### Features

- **mindctl:** Enabled explicit only models and auto approval support ([703421b](https://github.com/storm-software/mindctl/commit/703421b))

## [0.1.23](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.23) (09/25/2026)

### Features

- **mindctl:** Added Anthropic model provider implementation logic ([a2edd2b](https://github.com/storm-software/mindctl/commit/a2edd2b))

## [0.1.11](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.11) (09/24/2026)

### Bug Fixes

- **mindctl:** Resolve issue triggering CLI release ([29aff7c](https://github.com/storm-software/mindctl/commit/29aff7c))

## [0.1.10](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.10) (09/24/2026)

### Bug Fixes

- **mindctl:** Update CLI to default to list command ([acd70ed](https://github.com/storm-software/mindctl/commit/acd70ed))

## [0.1.9](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.9) (09/24/2026)

### Bug Fixes

- **mindctl-npm:** Cleaned up README and CLI release processing ([a32a38f](https://github.com/storm-software/mindctl/commit/a32a38f))

## [0.1.7](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.7) (09/24/2026)

### Bug Fixes

- **mindctl:** Resolve issue with duplicate `provider` commands ([4eeaf8a](https://github.com/storm-software/mindctl/commit/4eeaf8a))

### Features

- **mindctl:** Added `debug` mode to the Mindctl application ([254164d](https://github.com/storm-software/mindctl/commit/254164d))
- **mindctl:** Added the `history` command and filter args ([f4e7e4d](https://github.com/storm-software/mindctl/commit/f4e7e4d))

## [0.1.6](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.6) (09/23/2026)

### Bug Fixes

- **mindctl:** Rename `provider list` command for consistency ([8d2343b](https://github.com/storm-software/mindctl/commit/8d2343b))

### Features

- **mindctl:** Added support for the `model` and `provider` commands ([4746396](https://github.com/storm-software/mindctl/commit/4746396))

## [0.1.4](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.4) (09/23/2026)

### Bug Fixes

- **mindctl:** Update logic to read from user's XDG config directory ([31275c2](https://github.com/storm-software/mindctl/commit/31275c2))

### Features

- **config:** configure neutral Laya classifier ([f85f95b](https://github.com/storm-software/mindctl/commit/f85f95b))
- **mindctl:** Added `config` commands to `mindctl` CLI application ([4c39f82](https://github.com/storm-software/mindctl/commit/4c39f82))

### Continuous Integration

- **monorepo:** Added Homebrew artifact release configuration ([0d668c3](https://github.com/storm-software/mindctl/commit/0d668c3))

## [0.1.3](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.3) (09/23/2026)

### Features

- **mindctl:** Added support for XDG user configuration files ([5ea63fd](https://github.com/storm-software/mindctl/commit/5ea63fd))

## [0.1.2](https://github.com/storm-software/mindctl/releases/tag/mindctl%400.1.2) (09/23/2026)

### Features

- **mindctl:** Update command-line application to use `cobra` and `viper` packages ([2159169](https://github.com/storm-software/mindctl/commit/2159169))
