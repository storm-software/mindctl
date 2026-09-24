const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");

const workspaceRoot = join(__dirname, "..", "..");

test("Mindctl declares the Nx Release publish contract", () => {
  const project = JSON.parse(
    readFileSync(join(workspaceRoot, "cmd", "mindctl", "project.json"), "utf8")
  );

  assert.deepEqual(project.targets["nx-release-publish"], {
    executor: "nx:run-commands",
    options: {
      command:
        "echo 'Mindctl artifacts are published by the router release workflow.'"
    }
  });
});
