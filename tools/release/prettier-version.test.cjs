const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const prettier = require("prettier");

const workspaceRoot = join(__dirname, "..", "..");
const versionPath = join(workspaceRoot, "cmd", "mindctl", "VERSION");

test("formats the VERSION manifest through the workspace Prettier configuration", async () => {
  const config = await prettier.resolveConfig(versionPath);
  const formatted = await prettier.format(readFileSync(versionPath, "utf8"), {
    absolutePath: versionPath,
    ...config
  });

  assert.equal(formatted, "0.1.0\n");
});
