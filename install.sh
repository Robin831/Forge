#!/usr/bin/env bash
set -euo pipefail

# Forge install script — Linux & macOS
# Usage: curl -fsSL https://raw.githubusercontent.com/Robin831/Forge/main/install.sh | bash
# Options (env vars):
#   VERSION     — install a specific version (e.g. VERSION=v0.5.0)
#   INSTALL_DIR — destination directory (default: ~/bin)

REPO="Robin831/Forge"
BINARY="forge"
INSTALL_DIR="${INSTALL_DIR:-$HOME/bin}"

# ── OS / arch detection ────────────────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) ;;
  *) echo "ERROR: Unsupported OS: $OS" >&2; exit 1 ;;
esac

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64)          ARCH="amd64" ;;
  aarch64|arm64)   ARCH="arm64" ;;
  *) echo "ERROR: Unsupported architecture: $ARCH_RAW" >&2; exit 1 ;;
esac

# ── Resolve version ────────────────────────────────────────────────────────────
if [ -z "${VERSION:-}" ]; then
  echo "Fetching latest Forge release..."
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | cut -d'"' -f4)"
fi

if [ -z "$VERSION" ]; then
  echo "ERROR: Could not determine latest version. Set VERSION env var to override." >&2
  exit 1
fi

# Strip leading 'v' for asset name matching
VERSION_NUM="${VERSION#v}"

# ── Skip if already current ────────────────────────────────────────────────────
if command -v "$BINARY" > /dev/null 2>&1; then
  INSTALLED_VERSION="$("$BINARY" version 2>/dev/null || true)"
  if echo "$INSTALLED_VERSION" | grep -qF "$VERSION_NUM"; then
    echo "Forge ${VERSION} is already installed — nothing to do."
    exit 0
  fi
fi

ASSET_NAME="${BINARY}_${VERSION_NUM}_${OS}_${ARCH}.zip"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
ASSET_URL="${BASE_URL}/${ASSET_NAME}"
CHECKSUM_URL="${BASE_URL}/checksums.txt"

# ── Download to temp dir ───────────────────────────────────────────────────────
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading Forge ${VERSION} (${OS}/${ARCH})..."
curl -fsSL -o "${TMP_DIR}/${ASSET_NAME}" "$ASSET_URL"
curl -fsSL -o "${TMP_DIR}/checksums.txt" "$CHECKSUM_URL"

# ── Verify SHA256 ──────────────────────────────────────────────────────────────
echo "Verifying checksum..."
EXPECTED_HASH="$(grep "${ASSET_NAME}" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
if [ -z "$EXPECTED_HASH" ]; then
  echo "ERROR: No checksum entry found for ${ASSET_NAME} in checksums.txt" >&2
  exit 1
fi

if command -v sha256sum > /dev/null 2>&1; then
  ACTUAL_HASH="$(sha256sum "${TMP_DIR}/${ASSET_NAME}" | awk '{print $1}')"
elif command -v shasum > /dev/null 2>&1; then
  ACTUAL_HASH="$(shasum -a 256 "${TMP_DIR}/${ASSET_NAME}" | awk '{print $1}')"
else
  echo "ERROR: Neither sha256sum nor shasum found — cannot verify download." >&2
  exit 1
fi

if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
  echo "ERROR: SHA256 checksum mismatch!" >&2
  echo "  expected: $EXPECTED_HASH" >&2
  echo "  actual:   $ACTUAL_HASH" >&2
  exit 1
fi

# ── Extract and install ────────────────────────────────────────────────────────
mkdir -p "$INSTALL_DIR"

echo "Installing Forge to ${INSTALL_DIR}/${BINARY}..."
unzip -o -q "${TMP_DIR}/${ASSET_NAME}" "${BINARY}" -d "$TMP_DIR"
install -m 0755 "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"

# ── PATH advice ────────────────────────────────────────────────────────────────
case ":${PATH}:" in
  *:"${INSTALL_DIR}":*) ;;
  *)
    echo ""
    echo "NOTE: ${INSTALL_DIR} is not in your PATH."
    echo "Add the following line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo ""
    echo "  export PATH=\"\$HOME/bin:\$PATH\""
    echo ""
    ;;
esac

# ── Verify installation ────────────────────────────────────────────────────────
echo "Installation complete:"
"${INSTALL_DIR}/${BINARY}" version
