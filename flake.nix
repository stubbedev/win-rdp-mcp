{
  description = "MCP server for remote control of a Windows desktop (Go)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  # CI (.github/workflows/flake.yml) pushes the built x86_64-linux closure to
  # this cache. Consumers get it automatically with --accept-flake-config, or by
  # copying these two lines into their own nix.conf. The cache is public, so no
  # token is needed to pull from it.
  nixConfig = {
    extra-substituters = [ "https://nix.stubbe.dev/default" ];
    extra-trusted-public-keys = [ "default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=" ];
  };

  outputs = { self, nixpkgs }:
    let
      # The version tracks the npm wrapper and the git tag automatically: it is
      # read from package.json, which `npm run release:*` bumps, and which the
      # Go binary also embeds. No manual edit needed here.
      version = (builtins.fromJSON (builtins.readFile ./package.json)).version;

      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        # buildGoLatestModule, not buildGoModule: go.mod asks for the current
        # language version, and nixpkgs' default `go` trails it by a release.
        win-rdp-mcp = pkgs.buildGoLatestModule {
          pname = "win-rdp-mcp";
          inherit version;
          src = self;

          # vendorHash is kept current by .github/workflows/flake.yml on any
          # change to go.mod / go.sum. To refresh it by hand, run
          # `just nix-vendor-hash`.
          vendorHash = "sha256-pm7HM4exZu4mcaYQDzHSHBdxgPEpoWJBzyJAA3hsVUI=";

          # The version comes from the embedded package.json at runtime, so no
          # -X main.Version wiring is needed here.
          ldflags = [ "-s" "-w" ];

          # The test suite is the merge gate, so the flake runs it too: a
          # `nix build` that succeeds means the same checks CI runs passed.
          doCheck = true;

          meta = {
            description = "MCP server for remote control of a Windows desktop";
            homepage = "https://github.com/stubbedev/win-rdp-mcp";
            license = pkgs.lib.licenses.mit;
            mainProgram = "win-rdp-mcp";
            # The server itself only does anything useful on Windows, but it
            # builds and tests everywhere — which is what lets CI check it.
            platforms = pkgs.lib.platforms.unix ++ pkgs.lib.platforms.windows;
          };
        };
        default = win-rdp-mcp;
      });

      apps = forAllSystems (pkgs: rec {
        win-rdp-mcp = {
          type = "app";
          program = "${self.packages.${pkgs.system}.win-rdp-mcp}/bin/win-rdp-mcp";
        };
        default = win-rdp-mcp;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_latest
            pkgs.gopls
            pkgs.gotools # goimports, and the rest of the x/tools binaries
            pkgs.go-tools # staticcheck
            pkgs.delve
            pkgs.just
            pkgs.nodejs_24 # the npm wrapper, the smoke test and `just tools`
            pkgs.zip
            pkgs.unzip
            pkgs.nixpkgs-fmt
          ];

          shellHook = ''
            echo "win-rdp-mcp ${version} dev shell — $(go version)"
            echo "run 'just' for the recipe list"
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}
