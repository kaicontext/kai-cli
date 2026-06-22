#!/bin/sh
set -e

# Kai installer — https://kaicontext.com
# Usage:
#   curl -sSL https://get.kaicontext.com | sh
#   curl -sSL https://get.kaicontext.com | VERSION=0.3.1 sh
#
# Installs to ~/.kai/bin by default (no sudo), like rustup/deno/bun, and adds
# it to your PATH. Override with KAI_INSTALL_DIR=/usr/local/bin (may need sudo).
#
# The single `kai` binary statically links the closed-source kai-core engine
# and is published to public GitHub Releases on kaicontext/kai-cli.

REPO="kaicontext/kai-cli"
INSTALL_DIR="${KAI_INSTALL_DIR:-$HOME/.kai/bin}"
BINARY="kai"
VERSION="${VERSION:-latest}"

# place_binary moves $1 to $INSTALL_DIR/$2 without sudo when the dir is
# user-writable (the default ~/.kai/bin always is); only falls back to sudo if
# the user pointed KAI_INSTALL_DIR at a protected location.
place_binary() {
    src="$1"; name="$2"
    mkdir -p "$INSTALL_DIR" 2>/dev/null || true
    if [ -w "$INSTALL_DIR" ]; then
        mv "$src" "${INSTALL_DIR}/${name}"
    else
        echo "  ${INSTALL_DIR} not writable — using sudo..."
        sudo mv "$src" "${INSTALL_DIR}/${name}"
    fi
}

# install_kit fetches the kit TUI agent alongside kai. Best-effort: any failure
# here never aborts the install (kai is the primary binary). Skip with
# KAI_SKIP_KIT=1. Served from app.kaicontext.com/dl/ (kailab-control proxies it
# from the private kai-tui release, Cloudflare-cached). Uses $os/$arch from main.
install_kit() {
    [ "${KAI_SKIP_KIT:-0}" = "1" ] && return 0
    kit_asset="kit-${os}-${arch}.gz"
    kit_url="https://app.kaicontext.com/dl/${kit_asset}"
    echo "  Installing kit (TUI agent)..."
    kdir="$(mktemp -d)"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$kit_url" -o "${kdir}/${kit_asset}" 2>/dev/null || {
            echo "  (kit not available for ${os}/${arch} yet — skipped; kai is installed)"
            rm -rf "$kdir"; return 0
        }
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$kit_url" -O "${kdir}/${kit_asset}" 2>/dev/null || {
            echo "  (kit not available for ${os}/${arch} yet — skipped; kai is installed)"
            rm -rf "$kdir"; return 0
        }
    fi
    if ! gunzip "${kdir}/${kit_asset}" 2>/dev/null || ! chmod +x "${kdir}/kit-${os}-${arch}" 2>/dev/null; then
        echo "  (kit install skipped)"; rm -rf "$kdir"; return 0
    fi
    place_binary "${kdir}/kit-${os}-${arch}" "kit"
    rm -rf "$kdir"
    echo "  kit installed to ${INSTALL_DIR}/kit"
}

# ensure_path adds INSTALL_DIR to the user's shell rc if it isn't already on
# PATH (rustup/deno/bun style — idempotent, guarded by a marker comment).
ensure_path() {
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) return 0 ;;  # already on PATH, nothing to do
    esac

    shell_name="$(basename "${SHELL:-sh}")"
    case "$shell_name" in
        zsh)  rc="$HOME/.zshrc";  line="export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
        bash) rc="$HOME/.bashrc"; line="export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
        fish) rc="$HOME/.config/fish/config.fish"; line="fish_add_path \"$INSTALL_DIR\"" ;;
        *)    rc="$HOME/.profile"; line="export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
    esac

    if [ -f "$rc" ] && grep -qF "$INSTALL_DIR" "$rc" 2>/dev/null; then
        :  # already present
    else
        mkdir -p "$(dirname "$rc")" 2>/dev/null || true
        printf '\n# Added by kai installer\n%s\n' "$line" >> "$rc"
        echo "  Added ${INSTALL_DIR} to PATH in ${rc}"
    fi
    echo "  Run this now (or restart your shell):  export PATH=\"$INSTALL_DIR:\$PATH\""
}

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
        url="https://github.com/${REPO}/releases/latest/download/${asset}"
    else
        url="https://github.com/${REPO}/releases/download/v${VERSION}/${asset}"
    fi

    echo "Installing kai ${VERSION} (${os}/${arch}) to ${INSTALL_DIR}..."

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

    place_binary "${tmpdir}/${BINARY}-${os}-${arch}" "${BINARY}"

    # kit (TUI agent) alongside kai — best-effort, never aborts the install.
    install_kit || true

    echo ""
    echo "kai ${VERSION} installed to ${INSTALL_DIR}/${BINARY}"

    # Make sure the install dir is on PATH (no sudo, user shell rc).
    ensure_path

    echo ""
    echo "Get started:"
    echo "  kai init"
    echo "  kai capture"
    echo "  kai status"
}

main
