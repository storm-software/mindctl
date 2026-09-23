{ pkgs, ... }:
{
  name = "storm-software/mindctl";

  packages = [pkgs.goreleaser];

  dotenv.enable = true;
  dotenv.filename = [
    ".env"
    ".env.local"
  ];
  dotenv.disableHint = true;
}
