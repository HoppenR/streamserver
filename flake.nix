{
  description = "Official Flake for Streamserver";

  inputs = {
    nixpkgs.url = "github:Nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      build-pkg =
        pkgs:
        pkgs.buildGoModule {
          pname = "streamserver";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-uJTm4l2iCUy7HTWnkFwXzE+Ls63v2gDWSixdTutB7dA=";
          meta = {
            description = "Go-based stream server";
            homepage = "https://github.com/HoppenR/streamserver";
            mainProgram = "streamserver";
          };
        };

      outputs = flake-utils.lib.eachDefaultSystem (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          packages = rec {
            streamserver = build-pkg pkgs;
            default = streamserver;
          };

          devShells.default = pkgs.mkShellNoCC {
            buildInputs = with pkgs; [
              go
              gofumpt
              gopls
            ];

            shellHook = /* bash */ ''
              export STREAMSERVER_HOME=$(git rev-parse --show-toplevel) || exit
              export XDG_CONFIG_DIRS="$STREAMSERVER_HOME/.nvim_config:$XDG_CONFIG_DIRS"
            '';
          };
        }
      );
    in
    outputs
    // {
      nixosModules.default =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.streamserver;
        in
        {
          options.services.streamserver = {
            enable = lib.mkEnableOption "Streamserver service";
            package = lib.mkPackageOption self.packages.${pkgs.stdenv.hostPlatform.system} "streamserver" { };
            port = lib.mkOption {
              type = lib.types.port;
              default = null;
            };
            domain = lib.mkOption {
              type = lib.types.str;
              default = null;
            };
            environmentFile = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              example = lib.literalExpression ''
                pkgs.writeText "streamserver-env" '''
                  CLIENT_ID="your-id"
                  CLIENT_SECRET="your-secret"
                  USER_NAME="twitch-admin"
                '''
              '';
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.streamserver = {
              description = "Streamserver Service";
              after = [ "network.target" ];
              wantedBy = [ "multi-user.target" ];
              serviceConfig = {
                CacheDirectory = "streamserver";
                DynamicUser = true;
                Environment = [
                  "XDG_CONFIG_HOME=/var/lib/streamserver"
                  "XDG_CACHE_HOME=/var/cache/streamserver"
                ];
                EnvironmentFile = lib.mkIf (cfg.environmentFile != null) cfg.environmentFile;
                ExecStart = ''
                  ${lib.getExe cfg.package} \
                    -a 127.0.0.1:${toString cfg.port} \
                    -e https://streams.${cfg.domain}/oauth-callback
                '';
                MemoryDenyWriteExecute = true;
                NoNewPrivileges = true;
                PrivateTmp = true;
                ProtectHome = true;
                ProtectProc = "invisible";
                ProtectSystem = "strict";
                Restart = "always";
                StateDirectory = "streamserver";
              };
            };
          };
        };
    };
}
