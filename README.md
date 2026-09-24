<div align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://public.storm-cdn.com/mindctl/media/banner-1280x427-dark.gif">
  <source media="(prefers-color-scheme: light)" srcset="https://public.storm-cdn.com/mindctl/media/banner-1280x427-light.gif">
<img src="https://public.storm-cdn.com/mindctl/media/banner-1280x427-dark.gif" width="100%" alt="Mindctl" />
</picture>
</div>
<br />

<br />
<div align="center">
<b>
<a href="https://stormsoftware.com" target="_blank">Website</a>  •
<a href="https://github.com/storm-software/mindctl" target="_blank">GitHub</a>  •
<a href="https://discord.gg/MQ6YVzakM5">Discord</a>  •  <a href="https://stormstack.github.io/stormstack/" target="_blank">Docs</a>  •  <a href="https://stormsoftware.com/contact" target="_blank">Contact</a>  •
<a href="https://github.com/storm-software/stack/issues/new?assignees=&labels=bug&template=bug-report.yml&title=Bug Report%3A+">Report a Bug</a>
</b>
</div>
<br />

**Mindctl** is an LLM router that uses built-in logic and a System 1 decision model like [Jev](https://typesafe.ai/blog/introducing-system-one-models-and-jev) or [Laya](https://huggingface.co/convaiinnovations/laya) to intelligently route requests to the appropriate LLM model based on the input prompt.

<br />

<h3 align="center">💻 Visit <a href="https://stormsoftware.com" target="_blank">stormsoftware.com</a> to stay up to date with this developer</h3>
<br />

[![Commitizen friendly](https://img.shields.io/badge/commitizen-friendly-brightgreen.svg?style=for-the-badge&logo=commitlint&color=1fb2a6)](http://commitizen.github.io/cz-cli/)&nbsp;![semantic-release](https://img.shields.io/badge/%20%20%F0%9F%93%A6%F0%9F%9A%80-semantic--release-e10079.svg?style=for-the-badge&color=1fb2a6)&nbsp;![GitHub Workflow Status (with event)](https://img.shields.io/github/actions/workflow/status/storm-software/mindctl/cr.yml?style=for-the-badge&logo=github-actions&color=1fb2a6)

<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->

> [!IMPORTANT] 
> This repository, and the apps, libraries, and tools contained within, is still in it's initial development phase. As a result, bugs and issues are expected with it's usage. When the main development phase completes, a proper release will be performed, the packages will be available through NPM (and other distributions), and this message will be removed. However, in the meantime, please feel free to report any issues you may come across.

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<div align="center">
<b>Be sure to ⭐ this repository on GitHub so you can keep up to date on any daily progress!</b>
</div>

<!-- START doctoc -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->

## Table of Contents

- [Install](#install)
  - [Native binary](#native-binary)
  - [Homebrew](#homebrew)
  - [Container](#container)
  - [Go Install](#go-install)
  - [From source](#from-source)
- [Configuration](#configuration)
  - [Debug router traces](#debug-router-traces)
  - [Model and provider availability](#model-and-provider-availability)
  - [Laya system 1 sidecar](#laya-system-1-sidecar)
- [Command Line Interface](#command-line-interface)
- [Development](#development)
  - [Build](#build)
  - [Development Server](#development-server)
  - [ChatGPT subscription routing](#chatgpt-subscription-routing)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [Support](#support)
- [License](#license)
- [Contributors ✨](#contributors-)

<!-- END doctoc -->

<br />

# Install

The following installation methods are available for setting up Mindctl:

## npm

Install the native CLI through npm:

```sh
npm install --global mindctl
mindctl version

npx mindctl version
```

The npm package includes the supported Linux, macOS, and Windows native
binaries. Gateway mode still requires a runtime configuration file and the
required environment variables.

## Native binary

Download the archive for your platform from the GitHub Release, verify it
against `checksums.txt`, unpack it, and run:

```sh
./mindctl version
./mindctl --config ./config.yaml
```

On Linux, run `sha256sum --ignore-missing --check checksums.txt`. On macOS,
compare `shasum -a 256 <archive>` to the matching `checksums.txt` entry.

## Homebrew

```sh
brew tap storm-software/mindctl https://github.com/storm-software/mindctl
brew install mindctl
```

## Container

Pull a versioned image and mount a runtime configuration file:

```sh
docker pull ghcr.io/storm-software/mindctl
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/mindctl/config.yaml:ro" \
  --env-file .env \
  ghcr.io/storm-software/mindctl
```

The image runs as a non-root user and contains no usable configuration,
credentials, encryption keys, or database. Mount a writable data volume and
configure its SQLite path in `config.yaml`.

The GHCR package is public after its first release: an organization owner must
set the package visibility to **Public** in GitHub Packages before advertising
the image. Confirm an anonymous `docker pull` succeeds for the release tag.

## Go Install

Install Mindctl using the Go toolchain:

```sh
go install github.com/storm-software/mindctl/cmd/mindctl@latest
```

## From source

```sh
devenv shell -- go run ./cmd/mindctl --config ./config.yaml
```

# Configuration

Mindctl reads one strict YAML document. Unknown fields and multiple YAML
documents are rejected. Start from
[`config.example.yaml`](config.example.yaml), set the environment variables it
names, and choose a writable SQLite database path.

When no `--config` argument is supplied, Mindctl resolves the configuration in
this order:

1. `${XDG_CONFIG_HOME}/mindctl/config.yaml`, when that file exists. When
   `XDG_CONFIG_HOME` is unset, Mindctl uses `~/.config/mindctl/config.yaml`.
2. `config.example.yaml`, for source-checkout usage when no user config exists.

An explicit `--config <path>` always takes precedence. The legacy
`-config <path>` spelling is also accepted. Installed binaries do not include
`config.example.yaml`, so create the user configuration before running one
without `--config`:

```sh
mindctl_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/mindctl"
mkdir -p "$mindctl_config_dir"
cp config.example.yaml "$mindctl_config_dir/config.yaml"
chmod 700 "$mindctl_config_dir"
chmod 600 "$mindctl_config_dir/config.yaml"
```

The example configuration references environment variables rather than storing
secret values. Export values for its gateway token, `LAYA_CLASSIFIER_TOKEN`, and encryption
key before startup. Keep the configuration and SQLite database in locations
the Mindctl process can read and write, respectively.

Container deployments should continue to mount the configuration explicitly at
`/etc/mindctl/config.yaml`, as shown above; the image command supplies that
path through its configuration flag.

## Debug router traces

Set top-level `debug: true`, export `MINDCTL_DEBUG=true`, or start the gateway
with `--debug` to write detailed JSONL traces to
`${XDG_CACHE_HOME:-$HOME/.cache}/mindctl/logs`. An explicit
`--debug=true|false` overrides `MINDCTL_DEBUG`, which overrides the YAML value.
Each gateway process creates a private trace file containing request IDs,
derived feature counts, classifier signals, policy candidates and rejections,
selected routes, provider attempts, and stream retries. Trace files exclude
prompt and response content, credentials, request headers, and raw provider
errors.

## Model and provider availability

Mindctl can keep the routable model allow-list separate from the gateway
catalog. The gateway reads
`${XDG_STATE_HOME:-$HOME/.local/state}/mindctl/providers.yaml` once during
startup and caches the resulting availability in memory. When the file is
absent, each model's `available` value in `config.yaml` remains authoritative.
When the file exists, only the models listed in it are available:

```yaml
providers:
  openai:
    - gpt-6-astra
    - gpt-6-sol
    - gpt-6-luna
    - gpt-5.6-sol
    - gpt-5.6-terra
    - gpt-5.6-luna
    - gpt-5.3-codex
    - gpt-5.3-codex-spark
  deepseek:
    - deepseek-flash
```

Use the catalog commands to inspect or update the file. The first update
initializes it from the current `available` values, and changes take effect in
the gateway after restart.

```sh
mindctl model list
mindctl model list --all
mindctl model enable openai.gpt-6-astra
mindctl model disable openai

mindctl provider list
mindctl provider list --all
mindctl provider enable openai
mindctl provider disable openai
```

## Laya system 1 sidecar

The Laya classifier is optional. To launch the local sidecar, set a shared
bearer token and start its opt-in Compose profile:

```sh
export LAYA_CLASSIFIER_TOKEN="$(openssl rand -hex 32)"
docker compose -f compose.laya.yaml --profile laya up --build -d
```

`compose.laya.yaml` keeps port 8091 inside the named `mindctl` Docker network;
it does not publish a host port. Run the gateway in that network and configure
the endpoint shown in [`config.example.yaml`](config.example.yaml):

```yaml
classifier:
  endpoint: http://laya:8091
  token_env: LAYA_CLASSIFIER_TOKEN
```

The sidecar requires `Authorization: Bearer <LAYA_CLASSIFIER_TOKEN>` on
`POST /v1/classify`. It pins `convaiinnovations/laya` and the
`typed-decisions` variant, downloads model files only on its first startup,
and retains them in the `laya-model-cache` volume. Normal Go and Python tests
do not download model weights.

For an operator-managed sidecar, point the same `classifier:` configuration at
an absolute HTTPS endpoint instead. It must implement the documented
`mindctl.classifier.v1` bearer-authenticated `/v1/classify` contract; Mindctl
continues to make deterministic routing decisions and falls back safely when
the endpoint is unavailable.

# Command Line Interface

The Mindctl command line interface provides several commands to manage the application, its configuration, models, and providers. A complete list of available commands can be found in the [CLI documentation](docs/cli/mindctl.md).

# Development

Enter the repository's development environment before running Go commands:

```sh
devenv shell -- go test ./...
```

More information can be found in the
[Mindctl documentation](https://storm-software.github.io/mindctl/docs/getting-started/installation).

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

## Build

Run `devenv shell -- goreleaser release --snapshot --clean` to build local
release archives in `dist/` without publishing them.

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

## Development Server

Run `devenv shell -- go run ./cmd/mindctl` to start the gateway with the home
configuration when present, or pass `--config ./config.yaml` to use a project-
local file explicitly.

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

## ChatGPT subscription routing

Mindctl can route Codex requests through the ChatGPT account already signed in
to Codex. Configure Mindctl with `chatgpt_oauth_passthrough` as shown in
[`config.example.yaml`](config.example.yaml) or your active configuration file,
then add this custom provider to
the Codex `config.toml`:

```toml
model = "mindctl-auto"
model_provider = "mindctl"

[model_providers.mindctl]
name = "Mindctl"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = true
env_http_headers = { "X-Mindctl-Token" = "MINDCTL_GATEWAY_TOKEN" }
```

`mindctl-auto` enables Mindctl's automatic routing. A concrete model name is an
explicit selection and must match a model ID in Mindctl's configured catalog.

Sign in to Codex with ChatGPT and export `MINDCTL_GATEWAY_TOKEN` with the same
gateway token supplied to Mindctl. Do not set `OPENAI_API_KEY` for this
provider. Codex owns OAuth login and token refresh; Mindctl forwards the
request-scoped credential only to the configured ChatGPT Codex endpoint.

Available models, workspace access, rate limits, and usage limits remain
subject to the selected ChatGPT account and subscription. The shipped
ChatGPT Pro catalog includes `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`,
`gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.3-codex`, and the
Pro-only research preview `gpt-5.3-codex-spark`. GPT-6 model access is
currently rolling out and may not yet appear for every Pro account.

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

# Roadmap

See the [open issues](https://github.com/storm-software/mindctl/issues) for a
list of proposed features (and known issues).

- [Top Feature Requests](https://github.com/storm-software/mindctl/issues?q=label%3Aenhancement+is%3Aopen+sort%3Areactions-%2B1-desc)
  (Add your votes using the 👍 reaction)
- [Top Bugs](https://github.com/storm-software/mindctl/issues?q=is%3Aissue+is%3Aopen+label%3Abug+sort%3Areactions-%2B1-desc)
  (Add your votes using the 👍 reaction)
- [Newest Bugs](https://github.com/storm-software/mindctl/issues?q=is%3Aopen+is%3Aissue+label%3Abug)

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

# Contributing

First off, thanks for taking the time to contribute! Contributions are what
makes the open-source community such an amazing place to learn, inspire, and
create. Any contributions you make will benefit everybody else and are **greatly
appreciated**.

Please try to create bug reports that are:

- _Reproducible._ Include steps to reproduce the problem.
- _Specific._ Include as much detail as possible: which version, what
  environment, etc.
- _Unique._ Do not duplicate existing opened issues.
- _Scoped to a Single Bug._ One bug per report.

Please adhere to this project's [code of conduct](.github/CODE_OF_CONDUCT.md).

You can use
[markdownlint-cli](https://github.com/storm-software/mindctl/markdownlint-cli) to
check for common markdown style inconsistency.

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

# Support

Reach out to the maintainer at one of the following places:

- [Contact](https://stormsoftware.com/contact)
- [GitHub discussions](https://github.com/storm-software/mindctl/discussions)
- <contact@stormsoftware.com>

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

# License

This project is licensed under the **Apache License 2.0**. Feel free to edit and
distribute this template as you like. If you have any specific questions, please
reach out to the Storm Software development team.

See [LICENSE](LICENSE) for more information.

<br />

[![FOSSA Status](https://app.fossa.io/api/projects/git%2Bgithub.com%2Fstorm-software%2Fmindctl.svg?type=large&issueType=license)](https://app.fossa.io/projects/git%2Bgithub.com%2Fstorm-software%2Fmindctl?ref=badge_large&issueType=license)

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

# Contributors ✨

Thanks goes to these wonderful people
([emoji key](https://allcontributors.org/docs/en/emoji-key)):

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tbody>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="http://www.sullypat.com/"><img src="https://avatars.githubusercontent.com/u/99053093?v=4?s=100" width="100px;" alt="Patrick Sullivan"/><br /><sub><b>Patrick Sullivan</b></sub></a><br /><a href="#design-sullivanpj" title="Design">🎨</a> <a href="https://github.com/storm-software/mindctl/commits?author=sullivanpj" title="Code">💻</a> <a href="#tool-sullivanpj" title="Tools">🔧</a> <a href="https://github.com/storm-software/mindctl/commits?author=sullivanpj" title="Documentation">📖</a> <a href="https://github.com/storm-software/mindctl/commits?author=sullivanpj" title="Tests">⚠️</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://tylerbenning.com/"><img src="https://avatars.githubusercontent.com/u/7265547?v=4?s=100" width="100px;" alt="Tyler Benning"/><br /><sub><b>Tyler Benning</b></sub></a><br /><a href="#design-tbenning" title="Design">🎨</a></td>
      <td align="center" valign="top" width="14.28%"><a href="http://stormsoftware.com"><img src="https://avatars.githubusercontent.com/u/149802440?v=4?s=100" width="100px;" alt="Stormie"/><br /><sub><b>Stormie</b></sub></a><br /><a href="#maintenance-stormie-bot" title="Maintenance">🚧</a></td>
    </tr>
  </tbody>
  <tfoot>
    <tr>
      <td align="center" size="13px" colspan="7">
        <img src="https://raw.githubusercontent.com/all-contributors/all-contributors-cli/1b8533af435da9854653492b1327a23a4dbd0a10/assets/logo-small.svg">
          <a href="https://all-contributors.js.org/docs/en/bot/usage">Add your contributions</a>
        </img>
      </td>
    </tr>
  </tfoot>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<!-- ALL-CONTRIBUTORS-LIST:END -->

This project follows the
[all-contributors](https://github.com/all-contributors/all-contributors)
specification. Contributions of any kind welcome!

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />

<hr />
<br />

<div align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://public.storm-cdn.com/storm-software/media/banner-1280x320-dark.webp">
  <source media="(prefers-color-scheme: light)" srcset="https://public.storm-cdn.com/storm-software/media/banner-1280x320-light.webp">
<img src="https://public.storm-cdn.com/storm-software/media/banner-1280x320-dark.webp" width="100%" alt="Storm Software" />
</picture>
</div>
<br />

<div align="center">
<a href="https://stormsoftware.com" target="_blank">Website</a>  •  <a href="https://stormsoftware.com/contact" target="_blank">Contact</a>  •  <a href="https://linkedin.com/in/patrick-sullivan-865526b0" target="_blank">LinkedIn</a>  •  <a href="https://medium.com/@pat.joseph.sullivan" target="_blank">Medium</a>  •  <a href="https://github.com/storm-software" target="_blank">GitHub</a>  •  <a href="https://keybase.io/sullivanp" target="_blank">OpenPGP Key</a>
</div>

<div align="center">
<b>Fingerprint:</b> F47F 1853 BCAD DE9B 42C8  6316 9FDE EC95 47FE D106
</div>
<br />

Storm Software is an open source software development organization and creator
of Acidic, StormStack and StormCloud.

Our mission is to make software development more accessible. Our ideal future is
one where anyone can create software without years of prior development
experience serving as a barrier to entry. We hope to achieve this via LLMs,
Generative AI, and intuitive, high-level data modeling/programming languages.

Join us on [Discord](https://discord.gg/MQ6YVzakM5) to chat with the team,
receive release notifications, ask questions, and get involved.

If this sounds interesting, and you would like to help us in creating the next
generation of development tools, please reach out on our
[website](https://stormsoftware.com/contact) or join our
[Slack](https://join.slack.com/t/storm-software/shared_invite/zt-2gsmk04hs-i6yhK_r6urq0dkZYAwq2pA)
channel!

<br />

<div align="center"><a href="https://stormsoftware.com" target="_blank"><picture><source media="(prefers-color-scheme: dark)" srcset="https://public.storm-cdn.com/storm-software/icons/circle-dark.webp"><source media="(prefers-color-scheme: light)" srcset="https://public.storm-cdn.com/storm-software/icons/circle-light.webp"><img src="https://public.storm-cdn.com/storm-software/icons/circle-dark.webp" width="200px" alt="Storm Software" /></picture></a></div>
<br />
<div align="center"><a href="https://stormsoftware.com" target="_blank"><picture><source media="(prefers-color-scheme: dark)" srcset="https://public.storm-cdn.com/misc/text/visit-us-dark.png"><source media="(prefers-color-scheme: light)" srcset="https://public.storm-cdn.com/misc/text/visit-us-light.png"><img src="https://public.storm-cdn.com/misc/text/visit-us-dark.png" height="90px" alt="Visit us at stormsoftware.com" /></picture></a></div>
<br />

<div align="right">[ <a href="#table-of-contents">Back to top ▲</a> ]</div>
<br />
<br />
