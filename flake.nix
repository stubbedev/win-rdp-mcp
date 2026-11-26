{
  description = "MCP server that controls a Windows desktop over RDP (Go)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
    devtools = {
      url = "github:stubbedev/flakes.nix";
      inputs.flake-parts.follows = "flake-parts";
    };
  };

  # CI (.github/workflows/flake.yml) pushes the built x86_64-linux closure to
  # this cache. Consumers get it automatically with --accept-flake-config, or by
  # copying these two lines into their own nix.conf. The cache is public, so no
  # token is needed to pull from it.
  nixConfig = {
    extra-substituters = [ "https://xilos.dev/c/stubbedev/main" ];
    extra-trusted-public-keys = [ "xilos.dev/stubbedev/main-1:397R6S49v/FSvP+mZ3HlKrVN1KXhgzHIEIHxA/UuG6Y=" ];
  };

  outputs = inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      imports = [
        ./nix/package.nix
        ./nix/shell.nix
      ];
    };
}
