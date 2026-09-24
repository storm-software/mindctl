import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { targets } from "../lib/targets.mjs";
import { stagePackage } from "../scripts/stage.mjs";

const packageRoot = fileURLToPath(new URL("..", import.meta.url));
const version = "1.2.3";

function archiveName(target, artifactVersion = version) {
  return `mindctl_${artifactVersion}_${target.goos}-${target.goarch}.${target.archiveExtension}`;
}

function createFixture({
  artifactVersion = version,
  emptyTarget,
  omit = [],
  payload = "native executable"
} = {}) {
  const fixtureRoot = mkdtempSync(join(tmpdir(), "mindctl-npm-stage-"));
  const sourceDir = join(fixtureRoot, "source");
  const distDir = join(fixtureRoot, "dist");
  const outDir = join(fixtureRoot, "out");

  mkdirSync(sourceDir);
  mkdirSync(distDir);
  writeFileSync(
    join(sourceDir, "mindctl"),
    emptyTarget?.executable === "mindctl" ? "" : payload
  );
  writeFileSync(
    join(sourceDir, "mindctl.exe"),
    emptyTarget?.executable === "mindctl.exe" ? "" : payload
  );

  for (const target of targets) {
    if (omit.includes(target.payloadPath)) {
      continue;
    }

    const archive = join(distDir, archiveName(target, artifactVersion));
    if (target.archiveExtension === "tar.gz") {
      execFileSync("tar", [
        "-czf",
        archive,
        "-C",
        sourceDir,
        target.executable
      ]);
    } else {
      execFileSync("zip", ["-q", archive, target.executable], {
        cwd: sourceDir
      });
    }
  }

  return { fixtureRoot, distDir, outDir };
}

test("stages every GoReleaser target into a clean publish directory", () => {
  const fixture = createFixture();

  try {
    stagePackage({
      sourceDir: packageRoot,
      distDir: fixture.distDir,
      outDir: fixture.outDir,
      version
    });

    for (const target of targets) {
      assert.equal(existsSync(join(fixture.outDir, target.payloadPath)), true);
    }

    const manifest = JSON.parse(
      readFileSync(join(fixture.outDir, "package.json"), "utf8")
    );
    assert.equal(manifest.version, version);
    assert.equal(manifest.bin.mindctl, "bin/mindctl.js");
    assert.equal(existsSync(join(fixture.outDir, "test")), false);
    assert.equal(existsSync(join(fixture.outDir, "scripts")), false);
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});

test("rejects a missing GoReleaser archive before staging", () => {
  const missingTarget = targets.find(
    target => target.platform === "darwin" && target.arch === "arm64"
  );
  const fixture = createFixture({ omit: [missingTarget.payloadPath] });

  try {
    assert.throws(
      () =>
        stagePackage({
          sourceDir: packageRoot,
          distDir: fixture.distDir,
          outDir: fixture.outDir,
          version
        }),
      new RegExp(archiveName(missingTarget))
    );
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});

test("rejects an archive with an empty executable", () => {
  const emptyTarget = targets.find(
    target => target.platform === "win32" && target.arch === "x64"
  );
  const fixture = createFixture({ emptyTarget });

  try {
    assert.throws(
      () =>
        stagePackage({
          sourceDir: packageRoot,
          distDir: fixture.distDir,
          outDir: fixture.outDir,
          version
        }),
      /archive member is empty/
    );
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});

test("stages snapshot archives with the requested package version", () => {
  const artifactVersion = "1.2.3-SNAPSHOT-test";
  const packageVersion = "1.2.4";
  const fixture = createFixture({ artifactVersion });

  try {
    stagePackage({
      sourceDir: packageRoot,
      distDir: fixture.distDir,
      outDir: fixture.outDir,
      version: packageVersion,
      artifactVersion
    });

    const manifest = JSON.parse(
      readFileSync(join(fixture.outDir, "package.json"), "utf8")
    );
    assert.equal(manifest.version, packageVersion);
    assert.equal(
      existsSync(join(fixture.outDir, "bin/linux-x64/mindctl")),
      true
    );
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});

test("accepts a distinct artifact version through the staging command", () => {
  const artifactVersion = "1.2.3-SNAPSHOT-command";
  const packageVersion = "1.2.4";
  const fixture = createFixture({ artifactVersion });

  try {
    const result = spawnSync(
      process.execPath,
      [
        join(packageRoot, "scripts", "stage.mjs"),
        "--dist",
        fixture.distDir,
        "--out",
        fixture.outDir,
        "--version",
        packageVersion,
        "--artifact-version",
        artifactVersion
      ],
      { encoding: "utf8" }
    );

    assert.equal(result.status, 0, result.stderr);
    const manifest = JSON.parse(
      readFileSync(join(fixture.outDir, "package.json"), "utf8")
    );
    assert.equal(manifest.version, packageVersion);
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});

test("stages executables larger than Node's default child-process buffer", () => {
  const fixture = createFixture({ payload: Buffer.alloc(2 * 1024 * 1024, 1) });

  try {
    stagePackage({
      sourceDir: packageRoot,
      distDir: fixture.distDir,
      outDir: fixture.outDir,
      version
    });

    assert.equal(
      readFileSync(join(fixture.outDir, "bin/linux-x64/mindctl")).length,
      2 * 1024 * 1024
    );
  } finally {
    rmSync(fixture.fixtureRoot, { force: true, recursive: true });
  }
});
