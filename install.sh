#!/usr/bin/env sh
# qumo installer script for Linux and macOS
# Usage: curl -fsSL https://raw.githubusercontent.com/qumo-dev/qumo/main/install.sh | sh

set -eu

# Terminal capabilities & ANSI styling
if [ -t 1 ]; then
    IS_TTY=1
    BOLD="\033[1m"
    DIM="\033[2m"
    CYAN="\033[36m"
    GREEN="\033[32m"
    YELLOW="\033[33m"
    RED="\033[31m"
    RESET="\033[0m"
else
    IS_TTY=0
    BOLD=""
    DIM=""
    CYAN=""
    GREEN=""
    YELLOW=""
    RED=""
    RESET=""
fi

info() {
    printf "  ${CYAN}◇${RESET} %s\n" "$*"
}

success() {
    printf "  ${GREEN}✔${RESET} %s\n" "$*"
}

warn() {
    printf "  ${YELLOW}▲${RESET} %s\n" "$*" >&2
}

error() {
    printf "  ${RED}✖${RESET} %s\n" "$*" >&2
    exit 1
}

# Download with animated braille spinner in interactive shells
download_animated() {
    URL="$1"
    OUTPUT="$2"
    MSG="$3"
    DONE_MSG="$4"

    if [ "$IS_TTY" -eq 1 ] && command -v curl >/dev/null 2>&1; then
        curl -fsSL "$URL" -o "$OUTPUT" &
        PID=$!
        i=0
        while kill -0 "$PID" 2>/dev/null; do
            case $((i % 10)) in
                0) f="⠋" ;; 1) f="⠙" ;; 2) f="⠹" ;; 3) f="⠸" ;; 4) f="⠼" ;;
                5) f="⠴" ;; 6) f="⠦" ;; 7) f="⠧" ;; 8) f="⠇" ;; 9) f="⠏" ;;
            esac
            printf "\r  ${CYAN}%s${RESET} %s" "$f" "$MSG"
            sleep 0.07
            i=$((i + 1))
        done
        wait "$PID"
        printf "\r\033[2K"
        success "$DONE_MSG"
    else
        printf "  %s\n" "$MSG"
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL "$URL" -o "$OUTPUT"
        elif command -v wget >/dev/null 2>&1; then
            wget -qO "$OUTPUT" "$URL"
        else
            error "Neither curl nor wget found. Please install curl or wget."
        fi
        success "$DONE_MSG"
    fi
}

http_get() {
    URL="$1"
    OUTPUT="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$URL" -o "$OUTPUT"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$OUTPUT" "$URL"
    else
        error "Neither curl nor wget found. Please install curl or wget."
    fi
}

# 1. Header & Platform Check
printf "\n  ${BOLD}${CYAN}qumo${RESET} ${DIM}· Media over QUIC Relay installer${RESET}\n\n"

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
    Linux)
        OS_NAME="linux"
        OS_LABEL="Linux"
        ;;
    Darwin)
        OS_NAME="darwin"
        OS_LABEL="macOS"
        ;;
    *)
        error "Unsupported operating system: $OS. On Windows, use install.ps1."
        ;;
esac

case "$ARCH" in
    x86_64|amd64)
        ARCH_NAME="amd64"
        ARCH_LABEL="x86_64"
        ;;
    arm64|aarch64)
        ARCH_NAME="arm64"
        ARCH_LABEL="arm64"
        ;;
    *)
        error "Unsupported CPU architecture: $ARCH. Supported architectures: x86_64, arm64."
        ;;
esac

info "Detected platform: ${BOLD}${OS_LABEL} (${ARCH_LABEL})${RESET}"

# 2. Resolve version
REQUESTED_VERSION="${QUMO_VERSION:-latest}"

if [ "$REQUESTED_VERSION" != "latest" ] && [ -n "$REQUESTED_VERSION" ]; then
    VERSION="${REQUESTED_VERSION#v}"
    TAG="v${VERSION}"
else
    # Try GitHub API first
    TAG=""
    if command -v curl >/dev/null 2>&1; then
        TAG=$(curl -fsSL -H "User-Agent: qumo-installer" https://api.github.com/repos/qumo-dev/qumo/releases/latest 2>/dev/null | grep '"tag_name":' | head -n1 | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/' || true)
    fi

    # Fallback to redirect inspection if API failed or rate-limited
    if [ -z "$TAG" ]; then
        if command -v curl >/dev/null 2>&1; then
            REDIRECT_URL=$(curl -Ls -o /dev/null -w "%{url_effective}" https://github.com/qumo-dev/qumo/releases/latest 2>/dev/null || true)
            TAG="${REDIRECT_URL##*/}"
        elif command -v wget >/dev/null 2>&1; then
            REDIRECT_URL=$(wget -S --max-redirect=0 https://github.com/qumo-dev/qumo/releases/latest 2>&1 | grep -i 'Location:' | tail -n1 | tr -d '\r' | awk '{print $2}' || true)
            TAG="${REDIRECT_URL##*/}"
        fi
    fi

    if [ -z "$TAG" ] || [ "$TAG" = "latest" ]; then
        error "Failed to resolve latest version from GitHub. Please set QUMO_VERSION explicitly (e.g. QUMO_VERSION=0.6.260906)."
    fi

    VERSION="${TAG#v}"
fi

success "Resolved version: ${BOLD}${VERSION}${RESET}"

INSTALL_DIR="${QUMO_INSTALL_DIR:-$HOME/.qumo/bin}"
TARGET_BIN="${INSTALL_DIR}/qumo"

# 3. Download and verify
ASSET_NAME="qumo_${VERSION}_${OS_NAME}_${ARCH_NAME}.tar.gz"
DOWNLOAD_URL="https://github.com/qumo-dev/qumo/releases/download/${TAG}/${ASSET_NAME}"
CHECKSUMS_URL="https://github.com/qumo-dev/qumo/releases/download/${TAG}/checksums.txt"

TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'qumo-install')"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

download_animated "$DOWNLOAD_URL" "${TMP_DIR}/${ASSET_NAME}" \
    "Downloading qumo CLI..." \
    "Downloaded release archive ${DIM}(${ASSET_NAME})${RESET}"

http_get "$CHECKSUMS_URL" "${TMP_DIR}/checksums.txt"

EXPECTED_SUM=$(grep " ${ASSET_NAME}\$" "${TMP_DIR}/checksums.txt" | awk '{print $1}' || true)
if [ -z "$EXPECTED_SUM" ]; then
    error "Could not find checksum for ${ASSET_NAME} in checksums.txt."
fi

if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_SUM=$(sha256sum "${TMP_DIR}/${ASSET_NAME}" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL_SUM=$(shasum -a 256 "${TMP_DIR}/${ASSET_NAME}" | awk '{print $1}')
else
    warn "Neither sha256sum nor shasum available; skipping checksum validation."
    ACTUAL_SUM="$EXPECTED_SUM"
fi

if [ "$ACTUAL_SUM" != "$EXPECTED_SUM" ]; then
    error "Checksum mismatch! Expected: ${EXPECTED_SUM}, Actual: ${ACTUAL_SUM}"
fi
success "Verified SHA-256 checksum"

tar -xzf "${TMP_DIR}/${ASSET_NAME}" -C "$TMP_DIR"
success "Extracted binary"

if [ ! -f "${TMP_DIR}/qumo" ]; then
    error "Release archive did not contain 'qumo' executable."
fi

mkdir -p "$INSTALL_DIR"
cp -f "${TMP_DIR}/qumo" "$TARGET_BIN"
chmod +x "$TARGET_BIN"
success "Installed qumo to ${DIM}${TARGET_BIN}${RESET}"

# Check PATH
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        SHELL_NAME="$(basename "${SHELL:-sh}")"
        PROFILE=""
        case "$SHELL_NAME" in
            zsh)
                PROFILE="$HOME/.zshrc"
                ;;
            bash)
                if [ -f "$HOME/.bashrc" ]; then
                    PROFILE="$HOME/.bashrc"
                else
                    PROFILE="$HOME/.bash_profile"
                fi
                ;;
        esac

        if [ -n "$PROFILE" ]; then
            if ! grep -q "$INSTALL_DIR" "$PROFILE" 2>/dev/null; then
                printf '\nexport PATH="%s:$PATH"\n' "$INSTALL_DIR" >> "$PROFILE"
                success "Added ${DIM}${INSTALL_DIR}${RESET} to ${PROFILE}"
            fi
        else
            warn "Please add ${INSTALL_DIR} to your PATH:"
            warn "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        fi
        ;;
esac

printf "\n"
printf "  ${GREEN}${BOLD}✨ Successfully installed qumo v%s!${RESET}\n\n" "$VERSION"
printf "  ${DIM}${BOLD}Get started:${RESET}\n"
printf "    ${CYAN}qumo playground${RESET}      ${DIM}# launch relay + embedded web demo${RESET}\n"
printf "    ${CYAN}qumo --help${RESET}          ${DIM}# explore available commands${RESET}\n\n"
