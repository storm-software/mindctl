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

import { defineConfig } from "@sourcebook/config";

export default defineConfig({
  homepage: "https://stormsoftware.com/projects/mindctl",
  useLocalOutput: false,
  displayName: "Mindctl",
  docs: {
    type: "application",
    variant: "cli",
    outputPath: "docs/cmd/mindctl",
    src: "cmd/mindctl"
  }
});
