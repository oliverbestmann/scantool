{ config, lib, pkgs, ... }:

let
  cfg = config.services.scantool;

  boolFlag = name: value: lib.optionals value [ "--${name}" ] ++ lib.optionals (!value) [ "--${name}=false" ];

  args =
    [ "--out" cfg.outDir ]
    ++ lib.optionals (cfg.workDir != null) [ "--work" cfg.workDir ]
    ++ lib.optionals (cfg.dbPath != null) [ "--db" cfg.dbPath ]
    ++ [
      "--scan-command" cfg.scanCommand
      "--scan-timeout" cfg.scanTimeout
      "--name-layout" cfg.nameLayout
      "--input" cfg.input
      "--grab" cfg.grab
      "--http" cfg.http
      "--log-level" cfg.logLevel
      "--keep-actions" (toString cfg.keepActions)
    ]
    ++ lib.optionals (cfg.mergeCommand != null) [ "--merge-command" cfg.mergeCommand ]
    ++ lib.optionals (cfg.device != null) [ "--device" cfg.device ]
    ++ boolFlag "web-control" cfg.webControl
    ++ cfg.extraArgs;
in
{
  options.services.scantool = {
    enable = lib.mkEnableOption "scantool, a daemon that turns USB keypad presses into scanned PDF documents";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.scantool;
      defaultText = lib.literalExpression "pkgs.scantool";
      description = "The scantool package to run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "scantool";
      description = "User account under which scantool runs.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "scantool";
      description = "Group under which scantool runs.";
    };

    extraGroups = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "input" "scanner" "lp" ];
      description = ''
        Supplementary groups granted to the service: `input` to read the
        keypad's `/dev/input/event*` nodes, `scanner`/`lp` to reach a USB or
        network scanner.
      '';
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/scantool";
      description = "Directory holding scantool's state (managed via systemd's StateDirectory).";
    };

    outDir = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.stateDir}/scans";
      defaultText = lib.literalExpression ''"''${config.services.scantool.stateDir}/scans"'';
      description = "Directory the finished PDF documents are stored in.";
    };

    workDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Directory for pages of open documents. Defaults to `<outDir>/.scantool-work`.";
    };

    dbPath = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "SQLite database for sessions and the action log. Defaults to `<outDir>/scantool.db`.";
    };

    scanCommand = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.package}/bin/scan-page.sh";
      defaultText = lib.literalExpression ''"''${config.services.scantool.package}/bin/scan-page.sh"'';
      description = "Command scanning one page, called as `<command> <output.pdf>`.";
    };

    scanTimeout = lib.mkOption {
      type = lib.types.str;
      default = "3m";
      description = "Abort a scan that takes longer than this (Go duration syntax).";
    };

    mergeCommand = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "pdfunite {{in}} {{out}}";
      description = "External merge command. Defaults to the built-in pdfcpu merger.";
    };

    nameLayout = lib.mkOption {
      type = lib.types.str;
      default = "20060102-150405";
      description = "Go time layout used for the output file name.";
    };

    input = lib.mkOption {
      type = lib.types.enum [ "evdev" "libinput" "stdin" ];
      default = "evdev";
      description = "Key source backend.";
    };

    device = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/dev/input/by-id/usb-PCsensor_MK321U-event-kbd";
      description = ''
        Comma separated input devices to listen on. Defaults to every
        detected keyboard, which is rarely what you want: name the keypad
        explicitly so the service can also grab it exclusively (see
        `grab`).
      '';
    };

    grab = lib.mkOption {
      type = lib.types.enum [ "auto" "yes" "no" ];
      default = "auto";
      description = ''
        Take exclusive control of the input devices (evdev only). `auto`
        grabs only devices named with `device`.
      '';
    };

    http = lib.mkOption {
      type = lib.types.str;
      default = ":8080";
      description = "Listen address of the status page, empty to disable.";
    };

    webControl = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Allow triggering the a, b and c actions from the status page.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
      description = "Log verbosity.";
    };

    keepActions = lib.mkOption {
      type = lib.types.int;
      default = 5000;
      description = "Action log entries to keep on startup, 0 to keep everything.";
    };

    environment = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        SCAN_RESOLUTION = "300";
        SCAN_MODE = "Color";
      };
      description = ''
        Extra environment variables for the service, forwarded to the scan
        command (e.g. `SCAN_RESOLUTION`, `SCAN_MODE`, `SCAN_DEVICE`,
        `SCAN_SOURCE`, see `scan-page.sh`).
      '';
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra command line arguments passed to scantool verbatim.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = builtins.dirOf cfg.stateDir == "/var/lib";
        message = "services.scantool.stateDir must be a direct subdirectory of /var/lib, since it is created via systemd's StateDirectory=.";
      }
    ];

    users.users = lib.mkIf (cfg.user == "scantool") {
      scantool = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.stateDir;
      };
    };

    users.groups = lib.mkIf (cfg.group == "scantool") {
      scantool = { };
    };

    systemd.services.scantool = {
      description = "scantool, scan documents from a USB keypad";
      after = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];

      environment = cfg.environment;

      serviceConfig = {
        Type = "exec";
        User = cfg.user;
        Group = cfg.group;
        SupplementaryGroups = cfg.extraGroups;

        StateDirectory = builtins.baseNameOf cfg.stateDir;
        StateDirectoryMode = "0750";
        WorkingDirectory = cfg.stateDir;

        ExecStart = "${cfg.package}/bin/scantool ${lib.escapeShellArgs args}";

        Restart = "always";
        RestartSec = "5s";

        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = "yes";
        ReadWritePaths = [ cfg.stateDir ];
      };
    };
  };
}
