const assert = require("node:assert/strict");
const test = require("node:test");

const GoVersionActions = require("./go-version-actions.cjs");

function createTree(files) {
  return {
    exists(path) {
      return files.has(path);
    },
    read(path, encoding) {
      const value = files.get(path);
      if (value === undefined) {
        return null;
      }
      return encoding ? value : Buffer.from(value);
    },
    write(path, value) {
      files.set(path, value);
    }
  };
}

test("reads and updates the Go project VERSION manifest", async () => {
  const files = new Map([["cmd/mindctl/VERSION", "0.1.0\n"]]);
  const tree = createTree(files);
  const actions = new GoVersionActions(
    { name: "router" },
    { name: "mindctl", data: { root: "cmd/mindctl" } },
    {
      manifestRootsToUpdate: [],
      preserveLocalDependencyProtocols: true
    }
  );

  await actions.init(tree);

  assert.deepEqual(actions.manifestsToUpdate, [
    {
      manifestPath: "cmd/mindctl/VERSION",
      path: "cmd/mindctl",
      preserveLocalDependencyProtocols: true
    }
  ]);
  assert.deepEqual(await actions.readCurrentVersionFromSourceManifest(tree), {
    currentVersion: "0.1.0",
    manifestPath: "cmd/mindctl/VERSION"
  });
  assert.deepEqual(await actions.updateProjectVersion(tree, "1.2.3"), [
    "New version 1.2.3 written to manifest: cmd/mindctl/VERSION"
  ]);
  assert.equal(files.get("cmd/mindctl/VERSION"), "1.2.3\n");
  assert.deepEqual(await actions.updateProjectDependencies(tree, {}, {}), []);
});
