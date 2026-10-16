# Application documentation

Use for `type: "application"`.

Read `src` for a single application. Read `backend` and `frontend` for an application with separate sides; each value may be one workspace-relative path or an array. If multiple fields are provided, inspect all of them and describe their relationship rather than treating them as separate unrelated applications.

Document what the application does, its entry points and main flows, how its frontend and backend communicate where applicable, and how a developer runs or builds it when the repository provides that information. Use actual manifests and configuration for commands and environment requirements. For multiple applications, give each identified application its own local documentation when `useLocalOutput` is true; use clear names in a shared output directory otherwise.

Inspect routes, screens, server handlers, background jobs, data models, manifests, configuration, tests, and existing examples as relevant. Follow a flow across files before describing it. Organize pages around tasks readers need to complete: understand the application, use its main features, develop it, and operate it. Start with a short overview and navigation; give a substantial flow or subsystem its own page when a single overview would be hard to scan. Link related pages instead of repeating setup or shared concepts.

Possible documentation sections include:

## Overview and capabilities

Explain who the application serves, its main capabilities, and the boundaries of each configured application. Identify the supported entry points, such as web routes, CLI commands, APIs, or scheduled jobs, from the source. If a capability is incomplete or behind a feature flag, describe that status only when the source establishes it.

## Getting started

List prerequisites, setup steps, configuration, and the commands to run, test, or build the application. Use the package scripts and checked-in configuration as the source of truth. Explain which services are needed and how a reader can confirm the application started when that information is available. Name required environment variables and their purpose without including secret values or inventing defaults.

## Main user flows

Walk through meaningful tasks in the order a user performs them. For each flow, identify its entry point, required inputs or permissions, important states, and visible result. Add screenshots or examples when available and useful. Cover errors and recovery paths supported by the UI, handlers, or tests; do not infer behavior from route names alone.

## Architecture and data flow

Show the major frontend, backend, storage, and external-service pieces and how requests or events move between them. Document API endpoints, messages, or shared contracts only to the level verified by handlers, schemas, and callers. Explain data ownership and persistence where models or migrations support it. Keep implementation detail focused on what a developer needs to change or debug a flow.

## Configuration and integrations

Describe supported configuration options, environment variables, feature flags, authentication or authorization rules, and third-party integrations. Give the file or source that defines each contract, distinguish required from optional settings, and show safe examples when possible. Note deployment-specific values as such rather than presenting them as universal defaults.

## Development and operations

Document useful local workflows, test commands, build outputs, deployment or release steps, health checks, logs, and troubleshooting paths when the repository provides evidence for them. Separate documented commands from actions verified in a running environment. Link to existing operational documentation instead of copying procedures that have a separate owner.

Choose only sections that help this application and have supporting source. Use concise, runnable examples where practical, and mark unknown behavior or missing setup instructions as gaps rather than filling them with guesses.

## Structure

The following section outlines the documentation sections that should be created for each package within the repository.

**Note: Each documentation sections should be created in a separate markdown file within the repository.**

### Installation

Add this in a file named `installation.md`.

Provide instructions for installing the package, including any prerequisites and supported platforms. Include example commands for different package managers if applicable.

### Quick start

Add this in a file named `quick-start.md`.

Provide a small working example using the package, including the minimum required setup and expected result. Include any important notes or common pitfalls for new users.

### Configuration

Add this in a file named `configuration.md`.

Document public options, environment variables, plugins, adapters, callbacks, or customization hooks. Show valid values and precedence when implementation or tests define them. Describe what consumers must implement and how extensions are registered; omit speculative extension patterns.

### API reference

Add this in a file named `api-reference.md`.

Document the public functions, classes, components, hooks, commands, or types provided by the package. Include their signatures, required and optional inputs, defaults, return values, side effects, errors, and short usage examples when applicable. Include overloads, generics, asynchronous behavior, or lifecycle rules only when they affect correct use. Link to the owning export and avoid documenting internal symbols as supported interfaces.

### Compatibility and integration

Add this in a file named `integration.md`.

State supported runtimes, module formats, platform requirements, and version or peer dependency constraints from manifests and tests. Show how related packages integrate when public examples demonstrate it. Describe migration or breaking changes only when release notes, changelogs, or source history provide reliable evidence.

### Development and troubleshooting

Add this in a file named `troubleshooting.md`.

Include repository-supported build and test commands for contributors, plus common errors and their remedies when tests or existing documentation establish them. Keep consumer instructions distinct from contributor workflows. Link to existing release or maintenance guides when they are authoritative.

This section is optional and should be included only if the package has specific development or troubleshooting guidance that is not covered elsewhere.

### Contributing

Add this in a file named `contributing.md`.

Provide guidelines for contributing to the package, including how to report issues, submit pull requests, and follow the code of conduct. Include any specific requirements for commit messages, branching strategies, or review processes.

### License

Add this in a file named `license.md`.

Include information about the licensing of the package, including any restrictions or obligations for users and contributors.
