#!/bin/bash
set -e

# Install script for genie CLI
# Downloads the latest release binary from GitHub

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Map architecture names to match GoReleaser output
case "$ARCH" in
    x86_64)
        ARCH="x86_64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Map OS names to match GoReleaser output
case "$OS" in
    linux)
        OS="Linux"
        EXT="tar.gz"
        ;;
    darwin)
        OS="Darwin"
        EXT="tar.gz"
        ;;
    mingw*|msys*|cygwin*)
        OS="Windows"
        EXT="zip"
        ;;
    *)
        echo "Unsupported OS: $OS"
        exit 1
        ;;
esac

# Get latest release version
echo "Fetching latest release..."
VERSION=$(curl -s https://api.github.com/repos/sleuth-io/genie/releases/latest | grep '"tag_name"' | cut -d'"' -f4)

if [ -z "$VERSION" ]; then
    echo "Error: Could not fetch latest version"
    exit 1
fi

echo "Installing genie ${VERSION} for ${OS}_${ARCH}..."

# Build download URL
ARCHIVE_NAME="genie_${OS}_${ARCH}.${EXT}"
URL="https://github.com/sleuth-io/genie/releases/download/${VERSION}/${ARCHIVE_NAME}"

# Determine install location
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"

# Download and extract
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading ${URL}..."
curl -fsSL "$URL" -o "$TMPDIR/$ARCHIVE_NAME"

cd "$TMPDIR"
case "$EXT" in
    tar.gz) tar -xzf "$ARCHIVE_NAME" ;;
    zip)    unzip -q "$ARCHIVE_NAME" ;;
esac

mv genie "$INSTALL_DIR/genie"
chmod +x "$INSTALL_DIR/genie"

echo ""
echo "✓ genie installed to $INSTALL_DIR/genie"

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        echo ""
        echo "⚠ Warning: $INSTALL_DIR is not in your PATH"
        echo "Add this to your ~/.bashrc or ~/.zshrc:"
        echo "  export PATH=\"\$PATH:$INSTALL_DIR\""
        ;;
esac

echo ""
echo "Run 'genie --help' to get started."
