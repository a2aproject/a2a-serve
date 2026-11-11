#!/bin/sh
# Installs a2a-serve and the a2a CLI from their GitHub releases, checks their
# checksums, runs `a2a-serve doctor --fix`, and offers to set up an agent in
# the current directory.
#
#   curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
#
# Environment:
#   INSTALL_DIR        where the binaries go (default: ~/.local/bin)
#   A2A_SERVE_VERSION  a release tag such as v0.1.0 (default: the latest)
#   A2A_CLI_VERSION    a release tag of the a2a CLI (default: the latest)
#   A2A_NO_SETUP=1     skip doctor --fix and the setup question
#   A2A_CLI_ONLY=1     install only the a2a CLI, and skip doctor --fix and the
#                      setup question, for example to upgrade a2a on its own
set -eu

INSTALL_DIR=${INSTALL_DIR:-"$HOME/.local/bin"}
SERVE_REPO=https://github.com/a2aproject/a2a-serve
CLI_REPO=https://github.com/a2aproject/a2a-cli
# Overridable for tests: the base URL of a release's downloads, without the tag.
SERVE_DOWNLOAD=${A2A_SERVE_DOWNLOAD_URL:-"$SERVE_REPO/releases/download"}
CLI_DOWNLOAD=${A2A_CLI_DOWNLOAD_URL:-"$CLI_REPO/releases/download"}

say() { printf '%s\n' "$*"; }
fail() { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

platform() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "unsupported OS $(uname -s): download a release from $SERVE_REPO/releases" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "unsupported CPU $(uname -m)" ;;
  esac
}

# latest prints the tag of the latest release of a repository.
latest() {
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$1/releases/latest") || fail "can't reach $1"
  tag=${url##*/}
  case "$tag" in
    v*) printf '%s' "$tag" ;;
    *) fail "$1 has no release yet" ;;
  esac
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# fetch downloads <name>_<version>_<os>_<arch>.tar.gz of a release, checks it
# against the release's checksums.txt and installs the binary <name>.
fetch() {
  name=$1 base=$2 tag=$3
  archive="${name}_${tag#v}_${os}_${arch}.tar.gz"
  say "→ $name $tag"
  curl -fsSL -o "$tmp/$archive" "$base/$tag/$archive" || fail "can't download $base/$tag/$archive"
  curl -fsSL -o "$tmp/checksums-$name.txt" "$base/$tag/checksums.txt" || fail "can't download the checksums of $name $tag"
  want=$(grep " \**$archive\$" "$tmp/checksums-$name.txt" | cut -d' ' -f1)
  [ -n "$want" ] || fail "checksums.txt of $name $tag has no $archive"
  got=$(sha256 "$tmp/$archive")
  [ "$want" = "$got" ] || fail "checksum mismatch for $archive: want $want, got $got"
  mkdir -p "$tmp/$name"
  tar -xzf "$tmp/$archive" -C "$tmp/$name"
  bin=$(find "$tmp/$name" -type f -name "$name" | head -n 1)
  [ -n "$bin" ] || fail "$archive has no $name binary"
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$bin" "$INSTALL_DIR/$name"
  say "✓ installed $INSTALL_DIR/$name"
}

main() {
  need curl
  need tar
  platform
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM

  cli_tag=${A2A_CLI_VERSION:-$(latest "$CLI_REPO")}
  if [ "${A2A_CLI_ONLY:-}" != 1 ]; then
    serve_tag=${A2A_SERVE_VERSION:-$(latest "$SERVE_REPO")}
    fetch a2a-serve "$SERVE_DOWNLOAD" "$serve_tag"
  fi
  fetch a2a "$CLI_DOWNLOAD" "$cli_tag"

  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
      say "! $INSTALL_DIR is not on your PATH; add it, for example:"
      say "    export PATH=\"$INSTALL_DIR:\$PATH\""
      PATH="$INSTALL_DIR:$PATH"
      ;;
  esac

  [ "${A2A_NO_SETUP:-}" = 1 ] && return 0
  [ "${A2A_CLI_ONLY:-}" = 1 ] && return 0
  say ""
  # doctor exits non-zero when something is left to fix, such as Docker; it
  # says what, so the install carries on.
  "$INSTALL_DIR/a2a-serve" doctor --fix || true

  say ""
  if [ -r /dev/tty ] && [ -w /dev/tty ] && (exec </dev/tty) 2>/dev/null; then
    printf 'Set up an agent in %s now? [y/N] ' "$PWD" >/dev/tty
    read -r answer </dev/tty || answer=
    case "$answer" in
      y | Y | yes)
        exec "$INSTALL_DIR/a2a-serve" start </dev/tty >/dev/tty 2>&1
        ;;
    esac
  fi
  say "Set up an agent from a project directory with:"
  say "    cd ~/src/app && a2a-serve start"
}

main "$@"
