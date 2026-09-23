{
  description = "Mindctl router";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          lib = pkgs.lib;
          src = self.sourceInfo.outPath;
          version = lib.removeSuffix "\n" (builtins.readFile "${src}/cmd/mindctl/VERSION");
          commit = self.sourceInfo.rev or "dirty";
          date = self.sourceInfo.lastModifiedDate or "unknown";
          mindctl = pkgs.buildGoModule {
            pname = "mindctl";
            inherit version src;

            go = pkgs.go_1_26;
            subPackages = [ "cmd/mindctl" ];
            vendorHash = "sha256-8MCbBdii/V+mase7ZNNs3j4jX34MSp59ImKJlYDlHuI=";

            ldflags = [
              "-s"
              "-w"
              "-X main.version=${version}"
              "-X main.commit=${commit}"
              "-X main.date=${date}"
            ];

            meta = {
              description = "An LLM router that intelligently routes requests to the appropriate model";
              homepage = "https://stormsoftware.com/projects/mindctl";
              license = lib.licenses.asl20;
              mainProgram = "mindctl";
              platforms = systems;
            };
          };
        in
        {
          inherit mindctl;
          default = mindctl;
        }
      );
    };
}
