import assert from "node:assert/strict";
import test from "node:test";

import { launchMindctl } from "../lib/launch.mjs";
import { findTarget, supportedTargetLabels } from "../lib/targets.mjs";

test("maps every supported Node target to a distinct payload", () => {
  assert.equal(findTarget("linux", "x64").payloadPath, "bin/linux-x64/mindctl");
  assert.equal(
    findTarget("linux", "arm64").payloadPath,
    "bin/linux-arm64/mindctl"
  );
  assert.equal(
    findTarget("darwin", "x64").payloadPath,
    "bin/darwin-x64/mindctl"
  );
  assert.equal(
    findTarget("darwin", "arm64").payloadPath,
    "bin/darwin-arm64/mindctl"
  );
  assert.equal(
    findTarget("win32", "x64").payloadPath,
    "bin/win32-x64/mindctl.exe"
  );
});

test("forwards arguments and returns the native exit status", () => {
  const calls = [];
  const status = launchMindctl({
    args: ["--config", "router.yaml", "version"],
    platform: "linux",
    arch: "arm64",
    packageRoot: "/package",
    stderr: { write() {} },
    spawnSync: (...args) => {
      calls.push(args);
      return { status: 17 };
    }
  });

  assert.equal(status, 17);
  assert.deepEqual(calls[0][1], ["--config", "router.yaml", "version"]);
  assert.equal(calls[0][0], "/package/bin/linux-arm64/mindctl");
  assert.deepEqual(calls[0][2], {
    cwd: process.cwd(),
    env: process.env,
    stdio: "inherit"
  });
});

test("rejects unsupported hosts before invoking a binary", () => {
  const errors = [];
  let wasSpawned = false;

  const status = launchMindctl({
    args: [],
    platform: "freebsd",
    arch: "x64",
    packageRoot: "/package",
    stderr: { write: message => errors.push(message) },
    spawnSync: () => {
      wasSpawned = true;
      return { status: 0 };
    }
  });

  assert.equal(status, 1);
  assert.equal(wasSpawned, false);
  assert.match(errors.join(""), /freebsd\/x64/);
  assert.match(errors.join(""), new RegExp(supportedTargetLabels().join("|")));
});

test("reports a missing bundled executable", () => {
  const errors = [];
  const failure = Object.assign(new Error("spawn failed"), { code: "ENOENT" });

  const status = launchMindctl({
    args: ["version"],
    platform: "linux",
    arch: "x64",
    packageRoot: "/package",
    stderr: { write: message => errors.push(message) },
    spawnSync: () => ({ error: failure })
  });

  assert.equal(status, 1);
  assert.match(errors.join(""), /\/package\/bin\/linux-x64\/mindctl/);
  assert.match(errors.join(""), /ENOENT/);
});
