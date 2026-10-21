{
  perSystem = { controllerRuntimeDeps, pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
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
      ] ++ controllerRuntimeDeps;

      shellHook = ''
        echo "win-rdp-mcp ${self'.packages.default.version} dev shell — $(go version)"
        echo "run 'just' for the recipe list"
      '';
    };

    formatter = pkgs.nixpkgs-fmt;
  };
}
