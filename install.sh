#!/bin/sh
# gremlord installer — downloads the latest release binary to ~/.local/bin.
# Formerly agentic; your config and cost history are carried over on first run.
set -e

REPO="gremlord/gremlord"
# GREMLORD_INSTALL_DIR is the current name; AGENTIC_INSTALL_DIR still works
# for one release so existing install scripts keep placing the binary.
INSTALL_DIR="${GREMLORD_INSTALL_DIR:-${AGENTIC_INSTALL_DIR:-$HOME/.local/bin}}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "unsupported OS: $os (use install.ps1 on Windows)" >&2; exit 1 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required but not found." >&2
  case "$os" in
    darwin) echo "Install it with: brew install curl" >&2 ;;
    linux)  echo "Install it with your package manager, e.g.: sudo apt install curl" >&2 ;;
  esac
  exit 1
fi

# prompt_install checks whether $1 is on PATH; if missing, offers to install
# it by piping the installer at $3 into $4 (sh or bash — match what the
# tool's own docs specify). Reads the y/N answer from /dev/tty since stdin
# is usually the curl|sh pipe running this very script.
prompt_install() {
  bin="$1"; label="$2"; url="$3"; shell="$4"
  if command -v "$bin" >/dev/null 2>&1; then
    echo "✓ $label found"
    return 0
  fi
  echo "· $label not found"
  if [ -r /dev/tty ]; then
    printf "Install now via: %s ? [y/N] " "$url"
    # /dev/tty can fail to open even when -r reports it readable (e.g. no
    # controlling terminal in some CI/sandbox setups) — don't let `set -e`
    # abort the whole install over a declined prompt.
    reply=""
    read -r reply < /dev/tty 2>/dev/null || true
    case "$reply" in
      y|Y|yes|Yes) curl -fsSL "$url" | "$shell"; return $? ;;
    esac
  fi
  echo "  Install later with: curl -fsSL $url | $shell"
}

echo "Fetching latest release..."
tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
if [ -z "$tag" ]; then
  echo "could not determine latest release of $REPO" >&2
  exit 1
fi

asset="gremlord-$os-$arch"
url="https://github.com/$REPO/releases/download/$tag/$asset"
mkdir -p "$INSTALL_DIR"
echo "Downloading $asset ($tag)..."
curl -fsSL -o "$INSTALL_DIR/gremlord" "$url"
chmod +x "$INSTALL_DIR/gremlord"

echo "Installed gremlord $tag to $INSTALL_DIR/gremlord"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "NOTE: $INSTALL_DIR is not on your PATH — add it to your shell profile." ;;
esac

prompt_install claude "claude CLI" https://claude.ai/install.sh bash
prompt_install clauder "clauder (persistent memory, optional)" https://raw.githubusercontent.com/MaorBril/clauder/main/install.sh sh

echo "Next: gremlord setup"
