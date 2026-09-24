import { spawnSync as spawnSynchronously } from "node:child_process";
import { resolve } from "node:path";

import { findTarget, supportedTargetLabels } from "./targets.mjs";

export function launchMindctl({
  args,
  platform,
  arch,
  packageRoot,
  spawnSync = spawnSynchronously,
  stderr = process.stderr
}) {
  const target = findTarget(platform, arch);

  if (!target) {
    stderr.write(
      `mindctl: unsupported platform ${platform}/${arch}; supported platforms: ${supportedTargetLabels().join(", ")}\n`
    );
    return 1;
  }

  const binaryPath = resolve(packageRoot, target.payloadPath);
  const result = spawnSync(binaryPath, args, {
    cwd: process.cwd(),
    env: process.env,
    stdio: "inherit"
  });

  if (result.error) {
    const reason = result.error.code ?? result.error.message;
    stderr.write(`mindctl: cannot run ${binaryPath}: ${reason}\n`);
    return 1;
  }

  return result.status ?? 1;
}
