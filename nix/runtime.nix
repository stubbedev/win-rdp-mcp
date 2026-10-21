{
  perSystem = { lib, pkgs, ... }: {
    # The controller (`win-rdp-mcp control`) shells out to these at runtime to
    # hold the RDP session and drive the desktop. They exist only on Linux,
    # which is also the only platform the controller runs on; the agent half
    # needs none of them. The package wraps them onto its PATH and the dev
    # shell carries them for `just run` and live testing.
    _module.args.controllerRuntimeDeps = lib.optionals pkgs.stdenv.hostPlatform.isLinux [
      pkgs.freerdp # xfreerdp — the RDP client
      pkgs.xdotool # synthetic mouse/keyboard into the session
      pkgs.imagemagick # `import` — screen capture
      pkgs.xorg.xvfb # the headless X server the session is drawn into
      pkgs.xorg.xdpyinfo # readiness probe for the display
    ];
  };
}
