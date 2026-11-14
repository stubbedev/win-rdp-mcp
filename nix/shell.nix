{
  perSystem = { inputs', pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
      packages = [
        inputs'.devtools.packages.go
        pkgs.gopls
        pkgs.gotools # goimports, and the rest of the x/tools binaries
        pkgs.go-tools # staticcheck
        pkgs.delve
        pkgs.just
        pkgs.nodejs_24 # the npm wrapper and its release scripts
        pkgs.zip
        pkgs.unzip
        pkgs.nixpkgs-fmt
      ];

      shellHook = ''
        echo "win-rdp-mcp ${self'.packages.default.version} dev shell — $(go version)"
        echo "run 'just' for the recipe list"
      '';
    };

    formatter = pkgs.nixpkgs-fmt;
  };
}
