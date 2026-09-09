{
  description = "scantool - scan documents from a USB keypad";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        version = self.shortRev or self.dirtyShortRev or "dev";
      in
      {
        packages = rec {
          # Runtime tools scan-page.sh shells out to: scanimage from SANE, and
          # img2pdf.
          scanPageRuntimeInputs = [
            pkgs.sane-backends
            pkgs.img2pdf
          ];

          # Canon ScanGear MP v2 SANE backend (vendored proprietary blobs),
          # for PIXMA/MAXIFY models unsupported by sane-backends' own pixma
          # backend. Headless build has no GTK4/glib in its closure. Wire
          # either into services.scantool.extraSaneBackends to use it.
          scangearmp2Headless = pkgs.callPackage ./nix/packages/scangearmp2.nix { };
          scangearmp2 = pkgs.callPackage ./nix/packages/scangearmp2.nix { withGui = true; };

          scantool = pkgs.buildGoModule {
            pname = "scantool";
            inherit version;

            src = pkgs.lib.cleanSource ./.;

            vendorHash = "sha256-xANpKbWmRYrP7nhaXvl7LYyL4Siw2giGlIOeB5OfcXc=";

            # Pure Go build (modernc.org/sqlite has no cgo dependency).
            env.CGO_ENABLED = 0;

            # pdfcpu writes its config dir under $HOME; point it at a writable
            # directory for the test suite instead of the sandbox's
            # deliberately unwritable $HOME.
            preCheck = ''
              export HOME=$TMPDIR
            '';

            nativeBuildInputs = [ pkgs.makeWrapper ];

            postInstall = ''
              install -Dm755 ${./scan-page.sh} $out/bin/scan-page.sh
              wrapProgram $out/bin/scan-page.sh \
                --prefix PATH : ${pkgs.lib.makeBinPath scanPageRuntimeInputs}
            '';

            meta = with pkgs.lib; {
              description = "Daemon that turns USB keypad presses into scanned PDF documents";
              homepage = "https://github.com/oliverbestmann/scantool";
              license = licenses.mit;
              mainProgram = "scantool";
            };
          };

          default = scantool;
        };

        apps = rec {
          scantool = flake-utils.lib.mkApp { drv = self.packages.${system}.scantool; };
          default = scantool;
        };

        devShells.default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gofumpt pkgs.gopls ];
        };
      }) // {
        overlays.default = final: prev: {
          scantool = self.packages.${final.system}.default;
          scangearmp2Headless = self.packages.${final.system}.scangearmp2Headless;
          scangearmp2 = self.packages.${final.system}.scangearmp2;
        };

        nixosModules.default = { pkgs, ... }: {
          imports = [ ./nix/module.nix ];
          nixpkgs.overlays = [ self.overlays.default ];
        };
      };
}
