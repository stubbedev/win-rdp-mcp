{
  perSystem = { controllerRuntimeDeps, lib, pkgs, self', ... }: {
    packages = {
      # buildGoLatestModule, not buildGoModule: go.mod asks for the current
      # language version, and nixpkgs' default `go` trails it by a release.
      win-rdp-mcp = pkgs.buildGoLatestModule {
        pname = "win-rdp-mcp";
        # The version tracks the npm wrapper and the git tag automatically: it
        # is read from package.json, which `npm run release:*` bumps, and which
        # the Go binary also embeds. No manual edit needed here.
        inherit (lib.importJSON ../package.json) version;

        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };

        # vendorHash is kept current by .github/workflows/flake.yml on any
        # change to go.mod / go.sum. To refresh it by hand, run
        # `just nix-vendor-hash`.
        vendorHash = "sha256-zF/yqgJ5pmWJIBz5rMNseXhm/eX3lFrk0Y7au3rlUNI=";

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
        postBuild = lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
          GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
            -trimpath -ldflags "-s -w" -o $GOPATH/bin/win-rdp-mcp.exe .
        '';

        # Put the controller's runtime tools on the binary's PATH so
        # `nix run` works as a controller with nothing else installed.
        nativeBuildInputs = lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.makeWrapper ];
        postFixup = lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
          wrapProgram $out/bin/win-rdp-mcp \
            --prefix PATH : ${lib.makeBinPath controllerRuntimeDeps}
        '';

        meta = {
          description = "MCP server for remote control of a Windows desktop over RDP";
          homepage = "https://github.com/stubbedev/win-rdp-mcp";
          license = lib.licenses.mit;
          mainProgram = "win-rdp-mcp";
          # Builds and tests everywhere; the controller runs on Linux and
          # the agent on Windows.
          platforms = lib.platforms.unix ++ lib.platforms.windows;
        };
      };
      default = self'.packages.win-rdp-mcp;
    };

    apps = {
      win-rdp-mcp = {
        type = "app";
        program = lib.getExe self'.packages.win-rdp-mcp;
      };
      default = self'.apps.win-rdp-mcp;
    };
  };
}
