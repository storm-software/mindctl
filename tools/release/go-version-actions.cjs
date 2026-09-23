const { join } = require("node:path");
const { VersionActions } = require("nx/release");

module.exports = class GoVersionActions extends VersionActions {
  validManifestFilenames = ["VERSION"];

  async readCurrentVersionFromSourceManifest(tree) {
    const manifestPath = join(this.projectGraphNode.data.root, "VERSION");
    const currentVersion = tree.read(manifestPath, "utf-8")?.trim();

    if (!currentVersion) {
      throw new Error(
        `Unable to determine the current version for project "${this.projectGraphNode.name}" from ${manifestPath}`
      );
    }

    return { currentVersion, manifestPath };
  }

  async readCurrentVersionFromRegistry() {
    return null;
  }

  async readDependencies() {
    return [];
  }

  async readCurrentVersionOfDependency() {
    return { currentVersion: null, dependencyCollection: null };
  }

  async updateProjectVersion(tree, newVersion) {
    const messages = [];

    for (const manifest of this.manifestsToUpdate) {
      tree.write(manifest.manifestPath, `${newVersion}\n`);
      messages.push(
        `New version ${newVersion} written to manifest: ${manifest.manifestPath}`
      );
    }

    return messages;
  }

  async updateProjectDependencies() {
    return [];
  }
};
