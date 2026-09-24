{ pkgs, ... }:
{
  name = "storm-software/mindctl";

  dotenv.enable = true;
  dotenv.filename = [
    ".env"
    ".env.local"
  ];
  dotenv.disableHint = true;
}
