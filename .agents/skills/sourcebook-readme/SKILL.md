---
name: sourcebook-readme
description: Use when creating or updating a project's README.md for an application or library, especially its Quick Features, Installation, Usage, and Development sections.
license: Apache-2.0
metadata:
  author: storm-software
  version: "1.0.0"
---

# Sourcebook README

Write a useful entry point for users and contributors in the target project's `README.md`. This skill owns that file; use `sourcebook-docs` for other project documentation and `sourcebook-blog` for articles.

1. Identify the project from the request. In a monorepo, distinguish the root README from a package or app README before editing. Read the existing README, project manifests, source, examples, tests, and contributor instructions relevant to its claims. Preserve the existing voice, links, and unrelated sections.
2. Update or add these sections at the README's existing heading level, without duplicating equivalent sections:
   - **Quick Features:** A short, concrete list of capabilities supported by the current application or library. Avoid roadmap items and generic marketing claims.
   - **Installation:** Prerequisites and reproducible install or setup steps for a user. Distinguish published packages from source-checkout setup; do not claim a package or release exists without evidence.
   - **Usage:** The shortest working path from installation to a useful outcome. For an app, show how to run and use it; for a library, show a real import/API example. Include required configuration and commands only when supported by the project.
   - **Development:** Checkout setup, relevant scripts, tests, and local development tips for contributors. Keep contributor-only commands separate from user installation.
3. Use the project's actual package manager, script names, paths, and supported APIs. Check examples against source or existing tests; run inexpensive focused checks when practical. If a necessary detail cannot be established, say what remains unverified instead of inventing a command or example.
4. Maintain the surrounding README structure, including links and any table of contents. Regenerate marked, tool-owned sections with their generator rather than hand-editing them. Report what changed and any verification limits.

Do not require `sourcebook.config.*` for a README update; select the README from the user's project path or the inspected workspace.
