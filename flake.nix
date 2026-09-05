{
  description = "Codex subscription reverse proxy";

  nixConfig = {
    extra-substituters = ["https://sachahjkl.cachix.org"];
    extra-trusted-public-keys = ["sachahjkl.cachix.org-1:cepX7PCUV88hCchnh9prZM5V72wRkCf6oSJL6JfgWs0="];
  };

  inputs = {
    nixpkgs.url = "https://flakehub.com/f/NixOS/nixpkgs/0.2605";
    flake-utils.url = "https://flakehub.com/f/numtide/flake-utils/0.1";
    git-hooks = {
      url = "https://flakehub.com/f/cachix/git-hooks.nix/0.1";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
    git-hooks,
  }:
    flake-utils.lib.eachSystem ["x86_64-linux" "aarch64-linux"] (system: let
      pkgs = nixpkgs.legacyPackages.${system};
      src = pkgs.lib.fileset.toSource {
        root = ./.;
        fileset = pkgs.lib.fileset.unions [./go.mod (pkgs.lib.fileset.fileFilter (file: file.hasExt "go") ./.)];
      };
      package = pkgs.buildGoModule {
        pname = "codex-proxy";
        version = "0.1.0";
        inherit src;
        vendorHash = null;
        env.CGO_ENABLED = 0;
        ldflags = ["-s" "-w"];
        postInstall = ''
          mv "$out/bin/ai-api-proxy" "$out/bin/codex-proxy"
        '';
        meta.mainProgram = "codex-proxy";
      };
      goCheck =
        pkgs.runCommand "codex-proxy-go-check" {
          CGO_ENABLED = 1;
          nativeBuildInputs = [pkgs.go pkgs.stdenv.cc];
        } ''
          cp -r ${src} source
          chmod -R u+w source
          cd source
          export HOME="$TMPDIR"
          go vet ./...
          go test -race -timeout 120s ./...
          touch "$out"
        '';
      dockerImage = pkgs.dockerTools.buildLayeredImage {
        name = "codex-proxy";
        tag = package.version;
        contents = [package pkgs.cacert];
        fakeRootCommands = ''
          mkdir -p var/lib/codex-proxy
          chmod 0700 var/lib/codex-proxy
          chown 65532:65532 var/lib/codex-proxy
        '';
        enableFakechroot = true;
        config = {
          Cmd = ["${package}/bin/codex-proxy"];
          Env = [
            "LISTEN_ADDR=:8080"
            "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
          ];
          ExposedPorts."8080/tcp" = {};
          User = "65532:65532";
        };
      };
      preCommitCheck = git-hooks.lib.${system}.run {
        package = pkgs.prek;
        src = ./.;
        hooks = {
          actionlint.enable = true;
          alejandra.enable = true;
          check-added-large-files.enable = true;
          check-merge-conflicts.enable = true;
          check-json.enable = true;
          check-yaml.enable = true;
          deadnix.enable = true;
          end-of-file-fixer.enable = true;
          gofmt.enable = true;
          statix.enable = true;
          trim-trailing-whitespace.enable = true;
        };
      };
    in {
      packages = {
        default = package;
        inherit dockerImage;
      };

      apps.default = {
        type = "app";
        program = "${package}/bin/codex-proxy";
      };

      checks = {
        inherit dockerImage;
        build = package;
        go = goCheck;
        pre-commit = preCommitCheck;
        deployment = import ./tests/deployment.nix {
          inherit pkgs dockerImage package;
          module = self.nixosModules.default;
        };
      };

      devShells.default = pkgs.mkShell {
        CGO_ENABLED = 1;
        packages = preCommitCheck.enabledPackages ++ [pkgs.go pkgs.stdenv.cc];
        inherit (preCommitCheck) shellHook;
      };

      formatter = pkgs.alejandra;
    })
    // {
      nixosModules.default = {
        config,
        lib,
        pkgs,
        ...
      }: let
        cfg = config.services.codex-proxy;
      in {
        options.services.codex-proxy = {
          enable = lib.mkEnableOption "Codex subscription reverse proxy";
          package = lib.mkOption {
            type = lib.types.package;
            default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
            description = "Codex proxy package to run.";
          };
          listenAddress = lib.mkOption {
            type = lib.types.str;
            default = "127.0.0.1:8080";
            description = "Address and port the proxy listens on.";
          };
          publicUrl = lib.mkOption {
            type = lib.types.str;
            description = "Public HTTP or HTTPS origin advertised in the model catalog.";
          };
          allowUnauthenticated = lib.mkOption {
            type = lib.types.bool;
            default = false;
            description = "Allow requests without a proxy token on an access-controlled network.";
          };
          upstreamUrl = lib.mkOption {
            type = lib.types.str;
            default = "https://chatgpt.com/backend-api/codex";
            description = "HTTPS Codex backend URL.";
          };
          proxyTokenFile = lib.mkOption {
            type = lib.types.nullOr lib.types.path;
            default = null;
            description = "File that contains the shared proxy token.";
          };
          oauthCredentialFile = lib.mkOption {
            type = lib.types.path;
            description = "File that contains the seed ChatGPT OAuth credential.";
          };
        };

        config = lib.mkIf cfg.enable {
          assertions = [
            {
              assertion = cfg.proxyTokenFile != null || cfg.allowUnauthenticated;
              message = "codex-proxy requires proxyTokenFile unless allowUnauthenticated is enabled.";
            }
          ];
          systemd.services.codex-proxy = {
            description = "Codex subscription reverse proxy";
            after = ["network-online.target"];
            wants = ["network-online.target"];
            wantedBy = ["multi-user.target"];
            environment = {
              LISTEN_ADDR = cfg.listenAddress;
              PUBLIC_URL = cfg.publicUrl;
              ALLOW_UNAUTHENTICATED = lib.boolToString cfg.allowUnauthenticated;
              OAUTH_STATE_FILE = "/var/lib/codex-proxy/oauth.json";
              UPSTREAM_URL = cfg.upstreamUrl;
            };
            serviceConfig = {
              DynamicUser = true;
              ExecStart = lib.getExe cfg.package;
              LockPersonality = true;
              MemoryDenyWriteExecute = true;
              NoNewPrivileges = true;
              PrivateDevices = true;
              PrivateTmp = true;
              ProtectClock = true;
              ProtectControlGroups = true;
              ProtectHome = true;
              ProtectHostname = true;
              ProtectKernelLogs = true;
              ProtectKernelModules = true;
              ProtectKernelTunables = true;
              ProtectSystem = "strict";
              Restart = "on-failure";
              RestartSec = "5s";
              TimeoutStopSec = "25s";
              RestrictAddressFamilies = ["AF_INET" "AF_INET6"];
              RestrictNamespaces = true;
              RestrictRealtime = true;
              RestrictSUIDSGID = true;
              StateDirectory = "codex-proxy";
              StateDirectoryMode = "0700";
              SystemCallArchitectures = "native";
              SystemCallFilter = ["@system-service" "~@privileged" "~@resources"];
              UMask = "0077";
              Environment =
                ["OAUTH_CREDENTIAL_FILE=%d/oauth-credential"]
                ++ lib.optional (cfg.proxyTokenFile != null) "PROXY_TOKEN_FILE=%d/proxy-token";
              LoadCredential =
                ["oauth-credential:${cfg.oauthCredentialFile}"]
                ++ lib.optional (cfg.proxyTokenFile != null) "proxy-token:${cfg.proxyTokenFile}";
            };
          };
        };
      };
    };
}
