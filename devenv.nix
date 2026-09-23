{ pkgs, ... }:
{
  name = "storm-software/mindctl";

  packages = [pkgs.goreleaser pkgs.python312 pkgs.uv];

  dotenv.enable = true;
  dotenv.filename = [
    ".env"
    ".env.local"
  ];
  dotenv.disableHint = true;
}
