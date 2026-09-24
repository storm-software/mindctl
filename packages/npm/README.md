# Mindctl CLI

`mindctl` packages the native Mindctl command-line interface for npm users.
It includes the supported Linux, macOS, and Windows binaries, so installation
does not require Go or a download from GitHub Releases.

## Install

```sh
npm install --global mindctl
mindctl version
```

Or invoke the installed package without a global install:

```sh
npx mindctl version
```

The gateway still requires a runtime configuration file and the required
environment variables. See the repository README for configuration guidance.
