{
  description = "MCP server that controls a Windows desktop over RDP (Go)";

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
      packages = forAllSystems (pkgs:
        let
          # The controller (`win-rdp-mcp control`) shells out to these at
          # runtime to hold the RDP session and drive the desktop. They exist
          # only on Linux, which is also the only platform the controller runs
          # on; the agent half needs none of them.
          controllerRuntimeDeps = pkgs.lib.optionals pkgs.stdenv.isLinux [
            pkgs.freerdp # xfreerdp — the RDP client
            pkgs.xdotool # synthetic mouse/keyboard into the session
            pkgs.imagemagick # `import` — screen capture
            pkgs.xorg.xvfb # the headless X server the session is drawn into
            pkgs.xorg.xdpyinfo # readiness probe for the display
          ];
        in
        rec {
          # buildGoLatestModule, not buildGoModule: go.mod asks for the current
          # language version, and nixpkgs' default `go` trails it by a release.
          win-rdp-mcp = pkgs.buildGoLatestModule {
            pname = "win-rdp-mcp";
            inherit version;
            src = self;

            # vendorHash is kept current by .github/workflows/flake.yml on any
            # change to go.mod / go.sum. To refresh it by hand, run
            # `just nix-vendor-hash`.
            vendorHash = "sha256-agoMfQxeosUVhH5rCb3nYrwq7Ov25/ecXI4YXlKocNE=";

            # The version comes from the embedded package.json at runtime, so no
            # -X main.Version wiring is needed here.
            ldflags = [ "-s" "-w" ];

            # The test suite is the merge gate, so the flake runs it too: a
            # `nix build` that succeeds means the same checks CI runs passed.
            doCheck = true;

            # The controller pushes the Windows agent into the RDP session and
            # looks for it next to its own binary, so a Linux build has to carry
            # the cross-compiled .exe. Without it every agent-backed tool blocks
            # on a bootstrap that already failed and times out the caller.
            postBuild = pkgs.lib.optionalString pkgs.stdenv.isLinux ''
              GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
                -trimpath -ldflags "-s -w" -o $GOPATH/bin/win-rdp-mcp.exe .
            '';

            # Put the controller's runtime tools on the binary's PATH so
            # `nix run` works as a controller with nothing else installed.
            nativeBuildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.makeWrapper ];
            postFixup = pkgs.lib.optionalString pkgs.stdenv.isLinux ''
              wrapProgram $out/bin/win-rdp-mcp \
                --prefix PATH : ${pkgs.lib.makeBinPath controllerRuntimeDeps}
            '';

            meta = {
              description = "MCP server for remote control of a Windows desktop over RDP";
              homepage = "https://github.com/stubbedev/win-rdp-mcp";
              license = pkgs.lib.licenses.mit;
              mainProgram = "win-rdp-mcp";
              # Builds and tests everywhere; the controller runs on Linux and
              # the agent on Windows.
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
          ] ++ pkgs.lib.optionals pkgs.stdenv.isLinux [
            # The controller's runtime tools, so `just run`/live testing works
            # inside the dev shell.
            pkgs.freerdp
            pkgs.xdotool
            pkgs.imagemagick
            pkgs.xorg.xvfb
            pkgs.xorg.xdpyinfo
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
