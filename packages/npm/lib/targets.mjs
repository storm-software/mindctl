const targetRecords = [
  {
    platform: "linux",
    arch: "x64",
    goos: "linux",
    goarch: "amd64",
    archiveExtension: "tar.gz",
    executable: "mindctl",
    payloadPath: "bin/linux-x64/mindctl"
  },
  {
    platform: "linux",
    arch: "arm64",
    goos: "linux",
    goarch: "arm64",
    archiveExtension: "tar.gz",
    executable: "mindctl",
    payloadPath: "bin/linux-arm64/mindctl"
  },
  {
    platform: "darwin",
    arch: "x64",
    goos: "darwin",
    goarch: "amd64",
    archiveExtension: "tar.gz",
    executable: "mindctl",
    payloadPath: "bin/darwin-x64/mindctl"
  },
  {
    platform: "darwin",
    arch: "arm64",
    goos: "darwin",
    goarch: "arm64",
    archiveExtension: "tar.gz",
    executable: "mindctl",
    payloadPath: "bin/darwin-arm64/mindctl"
  },
  {
    platform: "win32",
    arch: "x64",
    goos: "windows",
    goarch: "amd64",
    archiveExtension: "zip",
    executable: "mindctl.exe",
    payloadPath: "bin/win32-x64/mindctl.exe"
  }
];

export const targets = Object.freeze(targetRecords.map(Object.freeze));

export function findTarget(platform, arch) {
  return targets.find(
    target => target.platform === platform && target.arch === arch
  );
}

export function supportedTargetLabels() {
  return targets.map(target => `${target.platform}/${target.arch}`);
}
