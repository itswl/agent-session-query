#!/bin/sh
# Installs asq: downloads the release for this machine, checks it against the release's
# published checksums, and puts the binary in ~/.local/bin — the user's own directory,
# no sudo. What asq reads is the installing user's sessions, so a system-wide install
# is a place to name, not the default.
#
#   curl -fsSL https://raw.githubusercontent.com/itswl/agent-session-query/main/install.sh | sh
#
#   VERSION=v1.2.0 sh install.sh                        a particular release instead of the latest
#   INSTALL_DIR=~/bin sh install.sh                     somewhere else than ~/.local/bin
#   INSTALL_DIR=/usr/local/bin sh install.sh            the system-wide place, which needs sudo
#   ASQ_REPO=you/fork sh install.sh                     releases of a fork
set -eu

REPO="${ASQ_REPO:-itswl/agent-session-query}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) echo "install.sh: this script installs on macOS and Linux; on Windows use install.ps1" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "install.sh: no release for $(uname -m) (there are amd64 and arm64)" >&2; exit 1 ;;
esac

version="${VERSION:-}"
if [ -z "$version" ]; then
  # The latest release redirects to its tag; the API is the fallback
  latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null || true)"
  version="${latest##*/}"
fi
if [ -z "$version" ] || [ "$version" = latest ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1 || true)"
fi
if [ -z "$version" ]; then
  echo "install.sh: could not find the latest release; name one: VERSION=v1.2.0 sh install.sh" >&2
  exit 1
fi

archive="asq-$os-$arch.tar.gz"
base="https://github.com/$REPO/releases/download/$version"
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t asq)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "asq $version for $os/$arch"
if ! curl -fsSL "$base/$archive" -o "$tmp/$archive"; then
  echo "install.sh: release $version of $REPO has no $archive — name another with VERSION=…" >&2
  exit 1
fi

# What was downloaded is about to be run: it is checked first, and a release without
# checksums is not installed.
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
expected="$(awk -v name="$archive" '$2 == name { print $1 }' "$tmp/SHA256SUMS")"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "install.sh: checksum mismatch for $archive" >&2
  echo "  published ${expected:-(none)}" >&2
  echo "  got       $actual" >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp"
chmod +x "$tmp/asq"

# The default directory is the user's own and always writable. One that was named and
# cannot be written — /usr/local/bin, say — takes sudo for the two commands that need it;
# with NO_SUDO it is an error, not a quiet install somewhere the reader did not name.
if mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/asq" "$INSTALL_DIR/asq"
elif [ -z "${NO_SUDO:-}" ] && command -v sudo >/dev/null 2>&1; then
  echo "installing to $INSTALL_DIR needs sudo"
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "$tmp/asq" "$INSTALL_DIR/asq"
else
  echo "install.sh: cannot write to $INSTALL_DIR — name a directory you can:" >&2
  echo "  INSTALL_DIR=~/bin sh install.sh" >&2
  exit 1
fi

echo "installed $INSTALL_DIR/asq"

# Say it when what was installed is not what a new shell will run.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "not on your PATH — add it: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
other="$(command -v asq 2>/dev/null || true)"
if [ -n "$other" ] && [ "$other" != "$INSTALL_DIR/asq" ]; then
  echo "another asq is on your PATH at $other — remove it, or it stays the one that runs"
fi
echo
echo "  asq ls          recent sessions"
echo "  asq serve       the page, at http://127.0.0.1:7717"
echo "  asq             everything else"
