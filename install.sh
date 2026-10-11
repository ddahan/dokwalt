#!/bin/sh
# DokWalt installer — macOS and Linux (Windows: run it inside WSL).
#
#   curl -fsSL https://raw.githubusercontent.com/ddahan/dokwalt/main/install.sh | sh
#
# Options (environment variables):
#   DOKWALT_VERSION=v0.1.0      install a specific release (default: latest)
#   DOKWALT_INSTALL_DIR=~/bin   install into this directory
#   DOKWALT_BASE_URL=...        download mirror (default: GitHub releases)
set -eu

REPO="ddahan/dokwalt"
VERSION="${DOKWALT_VERSION:-latest}"
if [ -n "${DOKWALT_BASE_URL:-}" ]; then
  BASE="$DOKWALT_BASE_URL"
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"; DIM="$(printf '\033[2m')"; PURPLE="$(printf '\033[38;5;141m')"
  GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RED="$(printf '\033[31m')"; RESET="$(printf '\033[0m')"
else
  BOLD=""; DIM=""; PURPLE=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi
say()  { printf '%s\n' "$*"; }
ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
die()  { printf '%s✗ %s%s\n' "$RED" "$*" "$RESET" >&2; exit 1; }

say "${PURPLE}${BOLD}◆ Installing DokWalt${RESET}"

# --- Platform --------------------------------------------------------------
os="$(uname -s)"
case "$os" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  MINGW*|MSYS*|CYGWIN*) die "Windows isn't supported natively — install WSL (https://learn.microsoft.com/windows/wsl/install) and run this command inside it." ;;
  *) die "Unsupported system: $os (DokWalt runs on macOS and Linux)." ;;
esac
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  armv7l|armv6l) die "32-bit ARM isn't supported. On a Raspberry Pi, install the 64-bit Raspberry Pi OS." ;;
  *) die "Unsupported CPU: $arch (amd64 and arm64 are supported)." ;;
esac
# Rosetta: an arm64 Mac running this shell under x86 emulation.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -in sysctl.proc_translated 2>/dev/null)" = 1 ]; then
  arch=arm64
fi
asset="dokwalt_${os}_${arch}"

# --- Download --------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "curl or wget is required."
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required to verify the download."
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
say "${DIM}  Downloading $asset ($VERSION)…${RESET}"
fetch "$BASE/$asset" "$tmp/dokwalt" || die "Download failed: $BASE/$asset"
fetch "$BASE/checksums.txt" "$tmp/checksums.txt" || die "Download failed: $BASE/checksums.txt"
want="$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/checksums.txt")"
got="$(sha256 "$tmp/dokwalt")"
[ -n "$want" ] || die "$asset is not listed in checksums.txt."
[ "$want" = "$got" ] || die "Checksum mismatch for $asset — the download is corrupted or was tampered with. Nothing was installed."
chmod +x "$tmp/dokwalt"
new_version="$("$tmp/dokwalt" version 2>/dev/null | awk '{print $2}')"
[ -n "$new_version" ] || die "The downloaded binary doesn't run on this machine."
ok "Downloaded and verified dokwalt $new_version for $os/$arch"

# --- Install location ------------------------------------------------------
# Prefer a directory that is already on PATH and writable without sudo.
on_path() { case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }
dir="${DOKWALT_INSTALL_DIR:-}"
use_sudo=""
if [ -z "$dir" ]; then
  for d in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin" "$HOME/bin"; do
    if on_path "$d" && [ -d "$d" ] && [ -w "$d" ]; then dir="$d"; break; fi
  done
fi
if [ -z "$dir" ]; then
  if command -v sudo >/dev/null 2>&1 && [ -r /dev/tty ]; then
    dir=/usr/local/bin
    use_sudo=sudo
    say "${DIM}  Installing to $dir requires your password (sudo).${RESET}"
  else
    dir="$HOME/.local/bin"
  fi
fi
dir="${dir%/}"
old_version=""
if [ -x "$dir/dokwalt" ]; then
  old_version="$("$dir/dokwalt" version 2>/dev/null | awk '{print $2}')" || true
fi
# sudo reads the password from the terminal even when this script is piped.
if [ -n "$use_sudo" ]; then
  # shellcheck disable=SC2024  # the redirect only gives sudo a terminal to prompt on
  sudo mkdir -p "$dir" </dev/tty
  # shellcheck disable=SC2024
  sudo install -m 0755 "$tmp/dokwalt" "$dir/dokwalt" </dev/tty
else
  mkdir -p "$dir"
  install -m 0755 "$tmp/dokwalt" "$dir/dokwalt"
fi
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$dir/dokwalt" 2>/dev/null || true

if [ -n "$old_version" ] && [ "$old_version" != "$new_version" ]; then
  ok "Updated dokwalt $old_version → $new_version in $dir"
else
  ok "Installed dokwalt $new_version in $dir"
fi

# --- Next steps ------------------------------------------------------------
if ! on_path "$dir"; then
  warn "$dir is not on your PATH yet. Add it with:"
  case "${SHELL:-}" in
    */fish) say "    fish_add_path $dir" ;;
    */zsh)  say "    echo 'export PATH=\"$dir:\$PATH\"' >> \"\$HOME/.zshrc\" && export PATH=\"$dir:\$PATH\"" ;;
    */bash) say "    echo 'export PATH=\"$dir:\$PATH\"' >> \"\$HOME/.bashrc\" && export PATH=\"$dir:\$PATH\"" ;;
    *)      say "    echo 'export PATH=\"$dir:\$PATH\"' >> \"\$HOME/.profile\" && export PATH=\"$dir:\$PATH\"" ;;
  esac
fi
other="$(command -v dokwalt 2>/dev/null || true)"
if [ -n "$other" ] && [ "$other" != "$dir/dokwalt" ]; then
  warn "Another dokwalt at $other comes first on your PATH — remove it to use this one."
fi
if ! command -v docker >/dev/null 2>&1; then
  warn "Docker isn't installed on this machine: DokWalt builds your images here."
  if [ "$os" = darwin ]; then
    say  "    Get Docker Desktop (https://www.docker.com/products/docker-desktop/) or OrbStack (https://orbstack.dev)."
  else
    say  "    Install Docker Engine: https://docs.docker.com/engine/install/ (on WSL: Docker Desktop with the WSL 2 backend)."
  fi
fi

say ""
if [ -n "$old_version" ] && [ "$old_version" != "$new_version" ]; then
  say "Upgrade your servers too:  ${BOLD}dokwalt server upgrade${RESET}"
else
  say "Next, install DokWalt on your server:"
  say "    ${BOLD}dokwalt server init you@your-server --email you@example.com${RESET}"
  say "Then, in your project folder:  ${BOLD}dokwalt apps:create myapp && dokwalt deploy${RESET}"
  say "Guides:  ${BOLD}dokwalt docs${RESET}"
fi
