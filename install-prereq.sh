#!/usr/bin/env bash
# install-prereq.sh — Install all prerequisites for KufiChain nodes
#
# Usage:
#   chmod +x install-prereq.sh
#   ./install-prereq.sh
#
# Installs: Docker CE, Docker Compose v2, jq, Go 1.21+, Fabric 2.5 binaries
# Supports: Ubuntu/Debian, CentOS/RHEL/Fedora, Alpine  (amd64 & arm64)

set -euo pipefail

# ── Versions ──────────────────────────────────────────────────────────
FABRIC_VERSION="2.5.12"
FABRIC_CA_VERSION="1.5.15"
GO_VERSION="1.21.13"
FABRIC_IMAGE_TAG="${FABRIC_VERSION%.*}" # keep Docker images aligned with Fabric v2.5.x
COUCHDB_VERSION="3.3.3"

# ── Colors ────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }

# ── Detect OS / Arch ─────────────────────────────────────────────────
detect_platform() {
    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux)  PLATFORM="linux" ;;
        Darwin) PLATFORM="darwin" ;;
        *)      fail "Unsupported OS: $OS" ;;
    esac

    case "$ARCH" in
        x86_64|amd64)
            GOARCH="amd64"
            COMPOSE_ARCH="x86_64"
            ;;
        aarch64|arm64)
            GOARCH="arm64"
            COMPOSE_ARCH="aarch64"
            ;;
        *)              fail "Unsupported architecture: $ARCH" ;;
    esac

    # Detect package manager
    if command -v apt-get &>/dev/null; then
        PKG_MGR="apt"
    elif command -v dnf &>/dev/null; then
        PKG_MGR="dnf"
    elif command -v yum &>/dev/null; then
        PKG_MGR="yum"
    elif command -v apk &>/dev/null; then
        PKG_MGR="apk"
    elif command -v brew &>/dev/null; then
        PKG_MGR="brew"
    else
        fail "No supported package manager found (apt, dnf, yum, apk, brew)"
    fi

    info "Platform: $PLATFORM/$GOARCH  Package manager: $PKG_MGR"
}

# ── Helpers ───────────────────────────────────────────────────────────
need_sudo() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "sudo"
    fi
}

version_gte() {
    # returns 0 when $1 >= $2
    local a b c x y z
    IFS='.' read -r a b c <<< "$1"
    IFS='.' read -r x y z <<< "$2"

    a=${a:-0}; b=${b:-0}; c=${c:-0}
    x=${x:-0}; y=${y:-0}; z=${z:-0}

    if [ "$a" -gt "$x" ]; then return 0; fi
    if [ "$a" -lt "$x" ]; then return 1; fi
    if [ "$b" -gt "$y" ]; then return 0; fi
    if [ "$b" -lt "$y" ]; then return 1; fi
    if [ "$c" -ge "$z" ]; then return 0; fi
    return 1
}

install_pkg() {
    local SUDO
    SUDO=$(need_sudo)
    case "$PKG_MGR" in
        apt)  $SUDO apt-get update -qq && $SUDO apt-get install -y -qq "$@" ;;
        dnf)  $SUDO dnf install -y -q "$@" ;;
        yum)  $SUDO yum install -y -q "$@" ;;
        apk)  $SUDO apk add --quiet "$@" ;;
        brew) brew install "$@" ;;
    esac
}

# ── 1. Docker ─────────────────────────────────────────────────────────
install_docker() {
    if command -v docker &>/dev/null; then
        ok "Docker already installed: $(docker --version)"
        return
    fi

    info "Installing Docker..."

    if [ "$PLATFORM" = "darwin" ]; then
        warn "On macOS, install Docker Desktop from https://docker.com/products/docker-desktop"
        warn "Then re-run this script."
        exit 1
    fi

    local SUDO
    SUDO=$(need_sudo)

    case "$PKG_MGR" in
        apt)
            $SUDO apt-get update -qq
            $SUDO apt-get install -y -qq ca-certificates curl gnupg lsb-release

            $SUDO install -m 0755 -d /etc/apt/keyrings
            if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
                curl -fsSL https://download.docker.com/linux/ubuntu/gpg | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg
            fi
            $SUDO chmod a+r /etc/apt/keyrings/docker.gpg

            # shellcheck disable=SC1091
            . /etc/os-release
            DISTRO="${ID:-ubuntu}"
            case "$DISTRO" in
                ubuntu|debian) ;;
                *)
                    if echo "${ID_LIKE:-}" | grep -qi debian; then
                        DISTRO="debian"
                    else
                        DISTRO="ubuntu"
                    fi
                    ;;
            esac
            CODENAME="${VERSION_CODENAME:-$(lsb_release -cs 2>/dev/null || echo stable)}"

            echo "deb [arch=$GOARCH signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$DISTRO $CODENAME stable" | \
                $SUDO tee /etc/apt/sources.list.d/docker.list > /dev/null

            $SUDO apt-get update -qq
            $SUDO apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin
            ;;
        dnf)
            $SUDO dnf install -y -q dnf-plugins-core
            $SUDO dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo 2>/dev/null || \
                $SUDO dnf config-manager --add-repo https://download.docker.com/linux/fedora/docker-ce.repo
            $SUDO dnf install -y -q docker-ce docker-ce-cli containerd.io docker-compose-plugin
            ;;
        yum)
            $SUDO yum install -y -q yum-utils
            $SUDO yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
            $SUDO $PKG_MGR install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
            ;;
        apk)
            $SUDO apk add docker docker-compose
            ;;
    esac

    # Start and enable Docker
    if command -v systemctl &>/dev/null; then
        $SUDO systemctl start docker
        $SUDO systemctl enable docker || true
    elif command -v rc-service &>/dev/null; then
        $SUDO rc-service docker start
        $SUDO rc-update add docker default || true
    else
        $SUDO service docker start 2>/dev/null || true
    fi

    # Add current user to docker group
    if [ "$(id -u)" -ne 0 ]; then
        $SUDO usermod -aG docker "$USER" 2>/dev/null || true
        warn "Added $USER to docker group. You may need to log out/in for this to take effect."
    fi

    ok "Docker installed: $(docker --version)"
}

# ── 2. Docker Compose v2 ─────────────────────────────────────────────
install_docker_compose() {
    if docker compose version &>/dev/null; then
        ok "Docker Compose v2 already installed: $(docker compose version --short 2>/dev/null || echo 'v2')"
        return
    fi

    info "Installing Docker Compose v2 plugin..."

    local SUDO
    SUDO=$(need_sudo)
    COMPOSE_URL="https://github.com/docker/compose/releases/latest/download/docker-compose-${PLATFORM}-${COMPOSE_ARCH}"

    mkdir -p "$HOME/.docker/cli-plugins" 2>/dev/null || true
    curl -fsSL "$COMPOSE_URL" -o "$HOME/.docker/cli-plugins/docker-compose"
    chmod +x "$HOME/.docker/cli-plugins/docker-compose"

    if docker compose version &>/dev/null; then
        ok "Docker Compose v2 installed"
    else
        # Try system-wide
        $SUDO mkdir -p /usr/local/lib/docker/cli-plugins 2>/dev/null || true
        $SUDO curl -fsSL "$COMPOSE_URL" -o /usr/local/lib/docker/cli-plugins/docker-compose
        $SUDO chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
        ok "Docker Compose v2 installed (system-wide)"
    fi
}

# ── 3. jq ─────────────────────────────────────────────────────────────
install_jq() {
    if command -v jq &>/dev/null; then
        ok "jq already installed: $(jq --version)"
        return
    fi

    info "Installing jq..."
    install_pkg jq
    ok "jq installed: $(jq --version)"
}

# ── 4. Go ─────────────────────────────────────────────────────────────
install_go() {
    if command -v go &>/dev/null; then
        CURRENT_GO="$(go env GOVERSION 2>/dev/null | sed -E 's/^go//')"
        CURRENT_GO="${CURRENT_GO%%[^0-9.]*}"
        if [ -n "$CURRENT_GO" ] && version_gte "$CURRENT_GO" "1.21.0"; then
            ok "Go already installed: $(go version)"
            return
        fi
        warn "Go ${CURRENT_GO:-unknown} found but need >= 1.21. Upgrading..."
    fi

    info "Installing Go ${GO_VERSION}..."

    local SUDO
    SUDO=$(need_sudo)
    GO_TAR="go${GO_VERSION}.${PLATFORM}-${GOARCH}.tar.gz"
    GO_URL="https://go.dev/dl/${GO_TAR}"

    curl -fsSL "$GO_URL" -o "/tmp/$GO_TAR"
    $SUDO rm -rf /usr/local/go
    $SUDO tar -C /usr/local -xzf "/tmp/$GO_TAR"
    rm -f "/tmp/$GO_TAR"

    # Add to PATH
    export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

    # Persist PATH
    if ! grep -q '/usr/local/go/bin' "$HOME/.bashrc" 2>/dev/null; then
        echo "export PATH=\"/usr/local/go/bin:\$HOME/go/bin:\$PATH\"" >> "$HOME/.bashrc"
    fi
    if ! grep -q '/usr/local/go/bin' "$HOME/.profile" 2>/dev/null; then
        echo "export PATH=\"/usr/local/go/bin:\$HOME/go/bin:\$PATH\"" >> "$HOME/.profile"
    fi

    ok "Go installed: $(go version)"
}

# ── 5. Fabric Binaries ───────────────────────────────────────────────
install_fabric_binaries() {
    # Check if already installed
    if command -v peer &>/dev/null && command -v cryptogen &>/dev/null && \
       command -v configtxgen &>/dev/null && command -v osnadmin &>/dev/null; then
        ok "Fabric binaries already installed: $(peer version 2>&1 | head -1 || echo 'found')"
        return
    fi

    info "Installing Hyperledger Fabric ${FABRIC_VERSION} binaries..."

    local SUDO
    SUDO=$(need_sudo)
    FABRIC_DEST="/usr/local/fabric/bin"
    $SUDO mkdir -p "$FABRIC_DEST"

    # Download fabric binaries
    FABRIC_TAR="hyperledger-fabric-${PLATFORM}-${GOARCH}-${FABRIC_VERSION}.tar.gz"
    FABRIC_URL="https://github.com/hyperledger/fabric/releases/download/v${FABRIC_VERSION}/${FABRIC_TAR}"

    info "  Downloading fabric binaries..."
    curl -fsSL "$FABRIC_URL" -o "/tmp/$FABRIC_TAR"
    $SUDO tar -C /usr/local/fabric -xzf "/tmp/$FABRIC_TAR" --strip-components=0
    rm -f "/tmp/$FABRIC_TAR"

    # Download fabric-ca binaries
    CA_TAR="hyperledger-fabric-ca-${PLATFORM}-${GOARCH}-${FABRIC_CA_VERSION}.tar.gz"
    CA_URL="https://github.com/hyperledger/fabric-ca/releases/download/v${FABRIC_CA_VERSION}/${CA_TAR}"

    info "  Downloading fabric-ca binaries..."
    curl -fsSL "$CA_URL" -o "/tmp/$CA_TAR" || warn "fabric-ca download failed (optional)"
    if [ -f "/tmp/$CA_TAR" ]; then
        $SUDO tar -C /usr/local/fabric -xzf "/tmp/$CA_TAR" --strip-components=0 2>/dev/null || true
        rm -f "/tmp/$CA_TAR"
    fi

    # Add to PATH
    export PATH="$FABRIC_DEST:$PATH"

    if ! grep -q "$FABRIC_DEST" "$HOME/.bashrc" 2>/dev/null; then
        echo "export PATH=\"$FABRIC_DEST:\$PATH\"" >> "$HOME/.bashrc"
    fi
    if ! grep -q "$FABRIC_DEST" "$HOME/.profile" 2>/dev/null; then
        echo "export PATH=\"$FABRIC_DEST:\$PATH\"" >> "$HOME/.profile"
    fi

    ok "Fabric binaries installed to $FABRIC_DEST"
}

# ── 6. Build KufiChain ───────────────────────────────────────────────
build_kufichain() {
    info "Building KufiChain..."

    SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    cd "$SCRIPT_DIR"

    if [ ! -f "go.mod" ]; then
        fail "go.mod not found. Run this script from the chain/ directory."
    fi

    if go build -o kufichain ./cmd/kufichain 2>/dev/null; then
        ok "Built: ./kufichain"
    else
        warn "Build skipped (cmd/kufichain not ready yet — run after setup)"
    fi

    if go build -o gateway ./cmd/gateway 2>/dev/null; then
        ok "Built: ./gateway"
    else
        warn "Build skipped (cmd/gateway not ready yet)"
    fi
}

# ── 7. Pull Docker Images ────────────────────────────────────────────
pull_fabric_images() {
    info "Pulling Fabric Docker images (this may take a few minutes)..."

    if ! docker info >/dev/null 2>&1; then
        warn "Docker daemon is not running; skipping image pre-pull"
        return
    fi

    if docker pull "hyperledger/fabric-peer:${FABRIC_IMAGE_TAG}" 2>/dev/null; then
        ok "  fabric-peer:${FABRIC_IMAGE_TAG}"
    else
        warn "  fabric-peer:${FABRIC_IMAGE_TAG} pull failed"
    fi
    if docker pull "hyperledger/fabric-orderer:${FABRIC_IMAGE_TAG}" 2>/dev/null; then
        ok "  fabric-orderer:${FABRIC_IMAGE_TAG}"
    else
        warn "  fabric-orderer:${FABRIC_IMAGE_TAG} pull failed"
    fi
    if docker pull "hyperledger/fabric-tools:${FABRIC_IMAGE_TAG}" 2>/dev/null; then
        ok "  fabric-tools:${FABRIC_IMAGE_TAG}"
    else
        warn "  fabric-tools:${FABRIC_IMAGE_TAG} pull failed"
    fi
    if docker pull "hyperledger/fabric-ccenv:${FABRIC_IMAGE_TAG}" 2>/dev/null; then
        ok "  fabric-ccenv:${FABRIC_IMAGE_TAG}"
    else
        warn "  fabric-ccenv:${FABRIC_IMAGE_TAG} pull failed"
    fi
    if docker pull "couchdb:${COUCHDB_VERSION}" 2>/dev/null; then
        ok "  couchdb:${COUCHDB_VERSION}"
    else
        warn "  couchdb:${COUCHDB_VERSION} pull failed"
    fi
}

# ── Summary ───────────────────────────────────────────────────────────
verify_all() {
    echo ""
    echo "══════════════════════════════════════════════════════"
    echo "  KufiChain Prerequisites Check"
    echo "══════════════════════════════════════════════════════"

    PASS=true
    for bin in docker jq go peer cryptogen configtxgen configtxlator osnadmin; do
        if command -v "$bin" &>/dev/null; then
            ok "  $bin"
        else
            echo -e "${RED}[MISS]${NC}  $bin"
            PASS=false
        fi
    done

    if docker compose version &>/dev/null; then
        ok "  docker compose v2"
    else
        echo -e "${RED}[MISS]${NC}  docker compose v2"
        PASS=false
    fi

    echo "══════════════════════════════════════════════════════"

    if [ "$PASS" = true ]; then
        echo ""
        echo -e "${GREEN}All prerequisites installed! You're ready to go:${NC}"
        echo ""
        echo "  ./kufichain setup              # Bootstrap a new network"
        echo "  ./kufichain join --bootstrap <IP:PORT>  # Join existing network"
        echo "  ./kufichain run                 # Start the gateway"
        echo ""
    else
        echo ""
        echo -e "${YELLOW}Some prerequisites are missing. Check the output above.${NC}"
        echo "You may need to log out and back in, then re-run this script."
        echo ""
    fi
}

# ── Main ──────────────────────────────────────────────────────────────
main() {
    echo ""
    echo "╔══════════════════════════════════════════════════════╗"
    echo "║     KufiChain — Prerequisites Installer             ║"
    echo "╚══════════════════════════════════════════════════════╝"
    echo ""

    detect_platform

    install_docker
    install_docker_compose
    install_jq
    install_go
    install_fabric_binaries
    pull_fabric_images
    build_kufichain

    verify_all
}

main "$@"
