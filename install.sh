#!/bin/sh
set -e

# Kai installer — https://kaicontext.com
# Usage:
#   curl -sSL https://get.kaicontext.com | sh
#   curl -sSL https://get.kaicontext.com | VERSION=0.3.1 sh

# Binaries are published to public GitHub Releases on the open-source CLI repo
# (kaicontext/kai-cli) by CI. The single `kai` binary statically links kai-core.
RELEASES_BASE="https://github.com/kaicontext/kai-cli/releases"
INSTALL_DIR="/usr/local/bin"
BINARY="kai"
VERSION="${VERSION:-latest}"

main() {
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"

    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *)
            echo "Error: unsupported architecture: $arch" >&2
            exit 1
            ;;
    esac

    case "$os" in
        linux) ;;
        darwin) ;;
        *)
            echo "Error: unsupported OS: $os" >&2
            exit 1
            ;;
    esac

    asset="${BINARY}-${os}-${arch}.gz"

    if [ "$VERSION" = "latest" ]; then
        url="${RELEASES_BASE}/latest/download/${asset}"
    else
        url="${RELEASES_BASE}/download/v${VERSION}/${asset}"
    fi

    echo "Installing kai ${VERSION} (${os}/${arch})..."

    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    echo "  Downloading ${url}..."
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "${tmpdir}/${asset}"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "${tmpdir}/${asset}"
    else
        echo "Error: curl or wget required" >&2
        exit 1
    fi

    echo "  Extracting..."
    gunzip "${tmpdir}/${asset}"
    chmod +x "${tmpdir}/${BINARY}-${os}-${arch}"

    # Install to INSTALL_DIR, use sudo if needed
    if [ -w "$INSTALL_DIR" ]; then
        mv "${tmpdir}/${BINARY}-${os}-${arch}" "${INSTALL_DIR}/${BINARY}"
    else
        echo "  Installing to ${INSTALL_DIR} (requires sudo)..."
        sudo mv "${tmpdir}/${BINARY}-${os}-${arch}" "${INSTALL_DIR}/${BINARY}"
    fi

    echo ""
    echo "kai ${VERSION} installed to ${INSTALL_DIR}/${BINARY}"
    echo ""

    # Run kai init if we're in a project directory
    if [ -d ".git" ] || [ -f "package.json" ] || [ -f "go.mod" ] || [ -f "Cargo.toml" ] || [ -f "requirements.txt" ] || [ -f "Makefile" ] || [ -f "pom.xml" ]; then
        echo "Running kai init..."
        echo ""
        kai init
    else
        echo "Run 'kai init' in your project to get started."
        echo ""
    fi
}

main
