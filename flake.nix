{
  description = "mcp-postgres-guard dev shell";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          # postgresql is only here so the integration tests can start a throwaway server.
          packages = with pkgs; [ go gopls golangci-lint postgresql ];
          CGO_ENABLED = "0";
        };
      });
    };
}
