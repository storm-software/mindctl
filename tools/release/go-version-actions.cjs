/* -------------------------------------------------------------------

                      🗲 Storm Software - Mindctl

 This code was released as part of the Mindctl project. Mindctl
 is maintained by Storm Software under the Apache-2.0 license, and is
 free for commercial and private use. For more information, please visit
 our licensing page at https://stormsoftware.com/licenses/projects/mindctl.

 Website:                  https://stormsoftware.com
 Repository:               https://github.com/storm-software/mindctl
 Documentation:            https://docs.stormsoftware.com/projects/mindctl
 Contact:                  https://stormsoftware.com/contact

 SPDX-License-Identifier:  Apache-2.0

 ------------------------------------------------------------------- */

/* eslint-disable unused-imports/no-unused-vars */

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
