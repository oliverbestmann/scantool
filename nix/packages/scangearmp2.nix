{
  lib,
  stdenv,
  fetchFromGitHub,
  cmake,
  pkg-config,
  gettext,
  intltool,
  binutils,
  autoPatchelfHook,
  wrapGAppsHook4,
  libusb1,
  sane-backends,
  libjpeg,
  gtk4,
  glib,
  # true  -> also builds the GTK4 scangearmp2 GUI binary.
  # false -> builds only the SANE backend (libsane-canon_pixma.so), which links
  #   solely against libjpeg/libusb/the Canon blobs - no GTK4/glib at all, for
  #   a much smaller closure suited to headless scanning via scanimage.
  withGui ? false,
}:

let
  # Vendor blobs are checked into the repo per architecture.
  vendorDirFor = {
    x86_64-linux = "usr/lib64";
    aarch64-linux = "usr/libaarch64";
  };
  vendorDir =
    vendorDirFor.${stdenv.hostPlatform.system}
      or (throw "scangearmp2: no vendored Canon blob dir for ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation {
  pname = "scangearmp2" + lib.optionalString (!withGui) "-headless";
  version = "unstable-2026-09-08";

  # Pinned upstream checkout, fetched directly from GitHub so this package is
  # self-contained. Bump `rev` to track upstream, then update `hash` (nix
  # will print the correct value if it doesn't match).
  src = fetchFromGitHub {
    owner = "ThierryHFR";
    repo = "scangearmp2";
    rev = "7aa87fb4570721900b1a5945d10ea829fadce55f";
    hash = "sha256-JnvfCwEJ/izlLeNm6VwawJCqnEmuafLiDR7XJzwKByU=";
  };

  nativeBuildInputs = [
    cmake
    pkg-config
    gettext
    intltool
    binutils # objdump, used at cmake-configure time on the vendor blobs
    autoPatchelfHook
  ]
  ++ lib.optional withGui wrapGAppsHook4;

  buildInputs = [
    libusb1
    sane-backends
    libjpeg

    # libstdc++/libgcc_s for the prebuilt Canon blobs
    stdenv.cc.cc.lib
  ]
  ++ lib.optionals withGui [
    gtk4
    glib
  ];

  # Only the vendor blob dir for the target architecture is needed; dropping
  # the others avoids pulling in cross-arch CMakeLists that can't build on
  # this platform.
  postPatch =
    ''
      # The project hardcodes /usr and writes several files straight to
      # the live system's /etc - fix it up to respect the Nix prefix.
      substituteInPlace CMakeLists.txt \
        --replace-fail 'set(CMAKE_INSTALL_PREFIX "/usr")' "" \
        --replace-fail 'DESTINATION "/etc/udev/rules.d/"' 'DESTINATION "''${CMAKE_INSTALL_PREFIX}/etc/udev/rules.d/"' \
        --replace-fail 'DESTINATION "/etc/sane.d/dll.d/"' 'DESTINATION "''${CMAKE_INSTALL_PREFIX}/etc/sane.d/dll.d/"' \
        --replace-fail 'DESTINATION "/etc/sane.d/"' 'DESTINATION "''${CMAKE_INSTALL_PREFIX}/etc/sane.d/"' \
        --replace-fail 'DESTINATION "/etc/"' 'DESTINATION "''${CMAKE_INSTALL_PREFIX}/etc/"' \
        --replace-fail 'DESTINATION "/usr/share/applications/"' 'DESTINATION "''${CMAKE_INSTALL_PREFIX}/share/applications/"'

      for arch in lib32 lib64 libaarch64 libmips64; do
        if [ "usr/$arch" != "${vendorDir}" ]; then
          rm -rf "usr/$arch"
        fi
      done
    ''
    + lib.optionalString (!withGui) ''
      # Gate the GTK4 GUI binary (and its pkg_search_module(GTK4 ...)
      # requirement) behind -DBUILD_GUI so a headless build never needs to
      # see gtk4.pc. The SANE backend in src/sane/ doesn't use GTK4 at all,
      # so it's unaffected.
      sed -i '/pkg_search_module(GTK4 REQUIRED gtk4)/i option(BUILD_GUI "Build the GTK4 GUI application" ON)\nif(BUILD_GUI)' CMakeLists.txt
      sed -i '/pkg_search_module(GTK4 REQUIRED gtk4)/a endif()' CMakeLists.txt
      sed -i '/DESTINATION "''${CMAKE_INSTALL_PREFIX}\/share\/applications\/"/i if(BUILD_GUI)' CMakeLists.txt
      sed -i '/DESTINATION "''${CMAKE_INSTALL_PREFIX}\/share\/applications\/"/a endif()' CMakeLists.txt
      sed -i '1i if(BUILD_GUI)' src/CMakeLists.txt
      sed -i '/add_subdirectory(sane)/i endif()' src/CMakeLists.txt
    '';

  cmakeFlags = [
    "-DTARGET_ARCH=${stdenv.hostPlatform.uname.processor}"
    "-DBUILD_TESTING=OFF"
    "-DBUILD_GUI=${if withGui then "ON" else "OFF"}"
  ];

  # The vendor .so files ship without RPATH/RUNPATH; autoPatchelfHook (with
  # stdenv.cc.cc.lib in buildInputs above) wires them up to the store paths
  # for libstdc++/libgcc_s/libc/libpthread.
  dontStrip = true;

  meta = {
    description =
      if withGui then
        "Canon ScanGear MP v2 scanner GUI and SANE backend"
      else
        "Canon ScanGear MP v2 SANE backend only (headless, no GTK4)";
    homepage = "https://github.com/ThierryHFR/scangearmp2";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  }
  // lib.optionalAttrs withGui { mainProgram = "scangearmp2"; };
}
