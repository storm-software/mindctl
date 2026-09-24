#!/usr/bin/env node

import { fileURLToPath } from "node:url";

import { launchMindctl } from "../lib/launch.mjs";

const packageRoot = fileURLToPath(new URL("..", import.meta.url));

process.exitCode = launchMindctl({
  args: process.argv.slice(2),
  platform: process.platform,
  arch: process.arch,
  packageRoot
});
