#!/bin/sh
set -e

# One-line installer for agent-session-query
# https://github.com/itswl/agent-session-query

REPO="itswl/agent-session-query"
BINARY_NAME="agent-session-query"

# Target installation directory: /usr/local/bin (or ~/.local/bin if /usr/local/bin is unwritable and no sudo)
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# 1. Detect OS
OS="$(uname -s)"
case "$OS" in
  Darwin)
    GOOS="darwin"
    ;;
  Linux)
    GOOS="linux"
    ;;
  *)
    echo "[ERROR] Unsupported OS: $OS (only macOS and Linux are supported by this shell script)." >&2
    echo "For Windows, download the zip release from https://github.com/$REPO/releases" >&2
    exit 1
    ;;
esac

# 2. Detect Architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)
    GOARCH="amd64"
    ;;
  arm64|aarch64)
    GOARCH="arm64"
    ;;
  *)
    echo "[ERROR] Unsupported architecture: $ARCH (supported: amd64, arm64)." >&2
    exit 1
    ;;
esac

# 3. Resolve version (default to latest release)
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  # Try resolving the latest release tag via GitHub redirect
  LATEST_URL="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null || true)"
  VERSION="${LATEST_URL##*/}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ]; then
  # Fallback to GitHub API if redirect failed
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)"
fi

if [ -z "$VERSION" ]; then
  echo "[ERROR] Failed to determine the latest release version." >&2
  echo "You can specify a version explicitly: VERSION=v0.23.0 sh -c '\$(curl -fsSL ...)'" >&2
  exit 1
fi

TARBALL="agent-session-query-${GOOS}-${GOARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$TARBALL"

echo "Downloading $BINARY_NAME $VERSION for ${GOOS}-${GOARCH}..."
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'asq')"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

curl -fsSL "$DOWNLOAD_URL" -o "$TMP_DIR/$TARBALL"

# Verify against the release's SHA256SUMS (published since v0.25.0). Releases older than
# that have no sums asset and install with a warning; a mismatch is always fatal.
SUMS_URL="https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
if curl -fsSL "$SUMS_URL" -o "$TMP_DIR/SHA256SUMS" 2>/dev/null; then
  EXPECTED="$(awk -v name="$TARBALL" '$2 == name { print $1 }' "$TMP_DIR/SHA256SUMS")"
  if [ -z "$EXPECTED" ]; then
    echo "[ERROR] SHA256SUMS has no entry for $TARBALL" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMP_DIR/$TARBALL" | awk '{print $1}')"
  else
    ACTUAL="$(shasum -a 256 "$TMP_DIR/$TARBALL" | awk '{print $1}')"
  fi
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "[ERROR] checksum mismatch for $TARBALL" >&2
    echo "  expected $EXPECTED" >&2
    echo "  got      $ACTUAL" >&2
    exit 1
  fi
  echo "Checksum verified."
else
  echo "[WARN] no SHA256SUMS published for $VERSION; installing without verification" >&2
fi

# Unpack
tar -xzf "$TMP_DIR/$TARBALL" -C "$TMP_DIR"

if [ ! -f "$TMP_DIR/$BINARY_NAME" ]; then
  echo "[ERROR] Archive did not contain $BINARY_NAME" >&2
  exit 1
fi

chmod +x "$TMP_DIR/$BINARY_NAME"

# Determine target directory and permissions
TARGET="$INSTALL_DIR/$BINARY_NAME"
if [ ! -d "$INSTALL_DIR" ]; then
  if [ -w "$(dirname "$INSTALL_DIR")" ]; then
    mkdir -p "$INSTALL_DIR"
  elif command -v sudo >/dev/null 2>&1; then
    sudo mkdir -p "$INSTALL_DIR"
  fi
fi

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP_DIR/$BINARY_NAME" "$TARGET"
else
  if command -v sudo >/dev/null 2>&1; then
    echo "Installing to $INSTALL_DIR (requires sudo)..."
    sudo mv "$TMP_DIR/$BINARY_NAME" "$TARGET"
  else
    # Fallback to ~/.local/bin
    ALT_DIR="$HOME/.local/bin"
    mkdir -p "$ALT_DIR"
    TARGET="$ALT_DIR/$BINARY_NAME"
    mv "$TMP_DIR/$BINARY_NAME" "$TARGET"
    echo "[INFO] $INSTALL_DIR is not writable; installed to $ALT_DIR instead."
    case ":$PATH:" in
      *":$ALT_DIR:"*) ;;
      *) echo "[NOTICE] Make sure $ALT_DIR is in your PATH: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
    esac
  fi
fi

echo "Successfully installed $BINARY_NAME ($VERSION) to $TARGET"
echo
echo "To get started:"
echo "  $BINARY_NAME --port 8080"
