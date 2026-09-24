import { execFileSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { targets } from "../lib/targets.mjs";

const extractionOptions = { maxBuffer: 100 * 1024 * 1024 };

function archiveName(target, version) {
  return `mindctl_${version}_${target.goos}-${target.goarch}.${target.archiveExtension}`;
}

function assertVersion(version) {
  if (
    !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(version)
  ) {
    throw new Error(
      `mindctl npm staging requires a semantic version, received ${version}`
    );
  }
}

function extractExecutable(target, archivePath) {
  const command = target.archiveExtension === "zip" ? "unzip" : "tar";
  const args =
    target.archiveExtension === "zip"
      ? ["-p", archivePath, target.executable]
      : ["-xOf", archivePath, target.executable];

  try {
    const executable = execFileSync(command, args, extractionOptions);
    return assertExecutable(executable);
  } catch (error) {
    if (target.archiveExtension === "zip" && error.code === "ENOENT") {
      try {
        return assertExecutable(
          execFileSync(
            "bsdtar",
            ["-xOf", archivePath, target.executable],
            extractionOptions
          )
        );
      } catch (fallbackError) {
        throw new Error(
          `mindctl npm staging could not extract ${target.executable} from ${archivePath}: ${fallbackError.message}`,
          { cause: fallbackError }
        );
      }
    }
    throw new Error(
      `mindctl npm staging could not extract ${target.executable} from ${archivePath}: ${error.message}`,
      { cause: error }
    );
  }
}

function assertExecutable(executable) {
  if (executable.length === 0) {
    throw new Error("archive member is empty");
  }
  return executable;
}

function copyPublishFiles(sourceDir, outDir, version) {
  const manifest = JSON.parse(
    readFileSync(join(sourceDir, "package.json"), "utf8")
  );
  manifest.version = version;
  delete manifest.scripts;

  mkdirSync(join(outDir, "bin"), { recursive: true });
  copyFileSync(
    join(sourceDir, "bin", "mindctl.js"),
    join(outDir, "bin", "mindctl.js")
  );
  cpSync(join(sourceDir, "lib"), join(outDir, "lib"), { recursive: true });
  copyFileSync(join(sourceDir, "README.md"), join(outDir, "README.md"));
  copyFileSync(
    resolve(sourceDir, "..", "..", "LICENSE"),
    join(outDir, "LICENSE")
  );
  writeFileSync(
    join(outDir, "package.json"),
    `${JSON.stringify(manifest, null, 2)}\n`
  );
}

export function stagePackage({
  sourceDir,
  distDir,
  outDir,
  version,
  artifactVersion = version
}) {
  assertVersion(version);
  assertVersion(artifactVersion);

  const archives = targets.map(target => ({
    target,
    path: join(distDir, archiveName(target, artifactVersion))
  }));

  for (const archive of archives) {
    if (!existsSync(archive.path)) {
      throw new Error(`mindctl npm staging is missing ${archive.path}`);
    }
  }

  rmSync(outDir, { force: true, recursive: true });
  mkdirSync(outDir, { recursive: true });
  copyPublishFiles(sourceDir, outDir, version);

  for (const archive of archives) {
    const executable = extractExecutable(archive.target, archive.path);
    const payloadPath = join(outDir, archive.target.payloadPath);
    mkdirSync(dirname(payloadPath), { recursive: true });
    writeFileSync(payloadPath, executable);

    if (archive.target.platform !== "win32") {
      chmodSync(payloadPath, 0o755);
    }
  }
}

function parseArguments(args) {
  const options = {};
  const optionNames = {
    "--artifact-version": "artifactVersion",
    "--dist": "dist",
    "--out": "out",
    "--version": "version"
  };

  for (let index = 0; index < args.length; index += 2) {
    const option = args[index];
    const value = args[index + 1];
    if (!optionNames[option] || !value) {
      throw new Error(
        "usage: stage.mjs --dist <directory> --out <directory> --version <semver> [--artifact-version <semver>]"
      );
    }
    options[optionNames[option]] = value;
  }

  if (!options.dist || !options.out || !options.version) {
    throw new Error(
      "usage: stage.mjs --dist <directory> --out <directory> --version <semver> [--artifact-version <semver>]"
    );
  }

  return options;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    const options = parseArguments(process.argv.slice(2));
    stagePackage({
      sourceDir: resolve(fileURLToPath(new URL("..", import.meta.url))),
      distDir: resolve(options.dist),
      outDir: resolve(options.out),
      version: options.version,
      artifactVersion: options.artifactVersion
    });
  } catch (error) {
    console.error(`mindctl npm staging: ${error.message}`);
    process.exitCode = 1;
  }
}
