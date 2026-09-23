const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { existsSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");

const workspaceRoot = join(__dirname, "..", "..");
const binaryPath = join(workspaceRoot, "dist", "apps", "mindctl", "mindctl");

test("Nx build injects the managed VERSION into the binary", () => {
  execFileSync(
    "pnpm",
    ["exec", "nx", "run", "mindctl:build", "--skip-nx-cache"],
    {
      cwd: workspaceRoot,
      stdio: "inherit"
    }
  );

  assert.equal(existsSync(binaryPath), true);
  assert.match(
    execFileSync(binaryPath, ["version"], { encoding: "utf-8" }),
    /^version=0\.1\.0$/m
  );
});
