#!/usr/bin/env bash
set -euo pipefail

# UltraBridge Installer
# Minimal configuration: port, username, password.
# All other configuration via Settings UI on first boot.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_PORT="8443"
DEFAULT_MCP_PORT="8081"
DEFAULT_SPC_PORT="8089"
DEFAULT_USERNAME="admin"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    cat <<EOF
Usage: install.sh [OPTIONS]

Minimal installer for UltraBridge. Prompts for ports, username, and password,
then starts the ultrabridge and ub-mcp containers. All other configuration is
done via the Settings UI on first boot.

Safe to re-run: overwrites generated config files each time.

Options:
  --fresh, -f     Clear the SQLite database before installing
                  (preserves other data in ultrabridge-data/)
  --nuke          Delete ALL UltraBridge data before installing
                  (removes entire ultrabridge-data/ directory)
  -y, --unattended  Non-interactive mode. Reads port, username, password from
                    environment variables: UB_PORT, UB_MCP_PORT, UB_SPC_PORT,
                    UB_USERNAME, UB_PASSWORD
  -h, --help      Show this help message

Prerequisites:
  - Docker and Docker Compose v2

Generated files (in current directory):
  docker-compose.yml      Compose service definition
  ultrabridge-data/       Persistent data directory
EOF
    exit 0
fi

# --- helpers ---

info()  { printf '\033[1;34m==> %s\033[0m\n' "$*"; }
ok()    { printf '\033[1;32m OK \033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m WARN \033[0m %s\n' "$*"; }
fail()  { printf '\033[1;31m FAIL \033[0m %s\n' "$*"; exit 1; }

prompt() {
    local var="$1" msg="$2" default="$3"
    if [[ "$UNATTENDED" == true ]]; then
        # In unattended mode, use existing env var value or default
        local current="${!var:-$default}"
        eval "$var=\"$current\""
        return
    fi
    local input
    if [[ -n "$default" ]]; then
        printf '%s [%s]: ' "$msg" "$default"
    else
        printf '%s: ' "$msg"
    fi
    read -r input
    eval "$var=\"${input:-$default}\""
}

prompt_password() {
    local var="$1" msg="$2"
    if [[ "$UNATTENDED" == true ]]; then
        # In unattended mode, use existing env var value
        if [[ -z "${!var:-}" ]]; then
            fail "Required variable $var is not set (unattended mode)"
        fi
        return
    fi
    local pw1 pw2
    while true; do
        printf '%s: ' "$msg"
        read -rs pw1
        echo
        printf 'Confirm password: '
        read -rs pw2
        echo
        if [[ "$pw1" == "$pw2" ]]; then
            if [[ -z "$pw1" ]]; then
                warn "Password cannot be empty. Try again."
                continue
            fi
            eval "$var=\"$pw1\""
            return
        fi
        warn "Passwords don't match. Try again."
    done
}

# --- parse arguments ---

FRESH_INSTALL=false
NUKE_INSTALL=false
UNATTENDED=false
for arg in "$@"; do
    case "$arg" in
        --fresh|-f) FRESH_INSTALL=true ;;
        --nuke) NUKE_INSTALL=true ;;
        -y|--unattended) UNATTENDED=true ;;
    esac
done

# --- pre-flight checks ---

info "UltraBridge Installer"
echo

# Docker
if ! command -v docker &>/dev/null; then
    fail "Docker is not installed. Install Docker first."
fi
ok "Docker found"

# Docker Compose
if ! docker compose version &>/dev/null; then
    fail "Docker Compose (v2) not found. Install docker-compose-plugin."
fi
ok "Docker Compose found"

echo

# --- fresh install ---

DATA_DIR="./ultrabridge-data"
if [[ "$NUKE_INSTALL" == true ]] || [[ "$FRESH_INSTALL" == true ]]; then
    # Stop containers if running
    docker stop ultrabridge ub-mcp 2>/dev/null || true
    if [[ "$NUKE_INSTALL" == true ]]; then
        warn "NUKE: deleting ALL UltraBridge data"
        rm -rf "$DATA_DIR"
        mkdir -p "$DATA_DIR"
        ok "All data deleted"
    elif [[ -f "$DATA_DIR/ultrabridge.db" ]]; then
        warn "Fresh install: clearing database"
        rm -f "$DATA_DIR/ultrabridge.db" "$DATA_DIR/ultrabridge.db-wal" "$DATA_DIR/ultrabridge.db-shm"
        rm -f "$DATA_DIR/ultrabridge-tasks.db" "$DATA_DIR/ultrabridge-tasks.db-wal" "$DATA_DIR/ultrabridge-tasks.db-shm"
        ok "Database cleared"
    fi
fi

# --- configuration ---

info "Configuration"
echo
echo "  Bootstrap configuration: port, username, password"
echo "  All other settings will be configured via the Settings UI on first boot."
echo

prompt UB_USERNAME "Username" "${UB_USERNAME:-$DEFAULT_USERNAME}"
prompt_password UB_PASSWORD "Password"
prompt UB_PORT "UltraBridge port to expose on host" "${UB_PORT:-$DEFAULT_PORT}"
prompt UB_MCP_PORT "MCP server port to expose on host" "${UB_MCP_PORT:-$DEFAULT_MCP_PORT}"
prompt UB_SPC_PORT "Supernote (SPC) device-sync port to expose on host" "${UB_SPC_PORT:-$DEFAULT_SPC_PORT}"

echo

# --- build image ---

info "Building UltraBridge Docker images..."

docker build --target ultrabridge -t ultrabridge:latest "$SCRIPT_DIR" || fail "ultrabridge image build failed"
docker build --target ub-mcp -t ub-mcp:latest "$SCRIPT_DIR" || fail "ub-mcp image build failed"

ok "Images built"

# --- generate docker-compose.yml ---

info "Generating docker-compose.yml"

mkdir -p "$DATA_DIR"

# Mount the Supernote storage tree if present. UltraBridge is the SPC server the
# device syncs against; this directory holds the SPC file root (e.g.
# /mnt/supernote/ub_sn_files) the device browses. UB no longer connects out to a
# `supernote-service` container, so there's no SPC network to join.
VOLUMES_BLOCK="      - ./ultrabridge-data:/data"
if [[ -d "/mnt/supernote" ]]; then
    ok "Detected Supernote directory at /mnt/supernote"
    VOLUMES_BLOCK="$VOLUMES_BLOCK
      - /mnt/supernote:/mnt/supernote"
fi
# Mount the reMarkable storage tree if present. UltraBridge is the reMarkable
# cloud the device syncs against; this holds the reMarkable source's data_path
# (documents + content-addressed blobs).
if [[ -d "/mnt/remarkable" ]]; then
    ok "Detected reMarkable directory at /mnt/remarkable"
    VOLUMES_BLOCK="$VOLUMES_BLOCK
      - /mnt/remarkable:/mnt/remarkable"
fi

cat > "$SCRIPT_DIR/docker-compose.yml" <<EOF
services:
  ultrabridge:
    image: ultrabridge:latest
    build:
      context: .
      dockerfile: Dockerfile
      target: ultrabridge
    container_name: ultrabridge
    ports:
      - "${UB_PORT}:8443"
      # Supernote device-sync (SPC) server. Only listens when
      # Settings > UB-as-SPC Device Sync Server > Mode = server.
      - "${UB_SPC_PORT}:8089"
    environment:
      - UB_DB_PATH=/data/ultrabridge.db
      - UB_LISTEN_ADDR=:8443
      - UB_TASK_DB_PATH=/data/ultrabridge-tasks.db
      - UB_MCP_PORT=${UB_MCP_PORT}
    volumes:
$VOLUMES_BLOCK
    restart: unless-stopped
  ub-mcp:
    image: ub-mcp:latest
    build:
      context: .
      dockerfile: Dockerfile
      target: ub-mcp
    container_name: ub-mcp
    ports:
      - "${UB_MCP_PORT}:8081"
    environment:
      - UB_MCP_API_URL=http://ultrabridge:8443
      - UB_MCP_API_USER=${UB_USERNAME}
      - UB_MCP_API_PASS=${UB_PASSWORD}
      - UB_DB_PATH=/data/ultrabridge.db
    volumes:
      - ./ultrabridge-data:/data
    depends_on:
      - ultrabridge
    command: ["--http", ":8081"]
    restart: unless-stopped
EOF

ok "Docker Compose file written"

echo

# --- start ---

info "Starting UltraBridge..."

# Remove any existing containers with these names (from prior install or manual docker run)
docker rm -f ultrabridge ub-mcp 2>/dev/null || true

docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d --force-recreate || fail "Failed to start containers"

# --- verify ---

HEALTH_URL="http://localhost:${UB_PORT}/health"
HEALTH_TIMEOUT=180
info "Waiting for health check (up to ${HEALTH_TIMEOUT}s)..."
HEALTH_OK=false
for i in $(seq 1 $HEALTH_TIMEOUT); do
    if curl -sf "$HEALTH_URL" >/dev/null 2>&1; then
        HEALTH_OK=true
        break
    fi
    # Show progress every 10 seconds
    if (( i % 10 == 0 )); then
        echo "  ... still waiting (${i}s)"
    fi
    sleep 1
done

if [[ "$HEALTH_OK" == true ]]; then
    ok "Health check passed (${i}s)"
else
    warn "Health check failed after ${HEALTH_TIMEOUT}s. Check logs:"
    echo "  docker compose logs ultrabridge"
    echo
    echo "  Common causes:"
    echo "  - Port $UB_PORT already in use"
    echo "  - Slow startup (database migration)"
    exit 1
fi

# --- seed user credentials ---

info "Seeding initial user credentials..."

docker exec ultrabridge ultrabridge seed-user "$UB_USERNAME" "$UB_PASSWORD" || fail "Failed to seed user credentials"

ok "User credentials saved"

# --- done ---

echo
info "UltraBridge is running!"
echo
echo "  Web UI:           http://localhost:${UB_PORT}/"
echo "  Health check:     http://localhost:${UB_PORT}/health"
echo
echo "  Username: $UB_USERNAME"
echo "  Password: (the one you just entered)"
echo
echo "  Complete configuration in the Settings UI."
echo
echo "  Supernote sync (SPC) is exposed on host port ${UB_SPC_PORT} (-> :8089)."
echo "  Enable it under Settings > UB-as-SPC Device Sync Server (Mode = server),"
echo "  then restart. Behind a reverse proxy it needs its OWN Supernote/SPC hostname"
echo "  routing to :8089, separate from the web UI host on ${UB_PORT}. For example:"
echo "    supernote.example.com -> :8089"
echo "    ub.example.com        -> :${UB_PORT}"
echo "  Do not route SPC under the UltraBridge web hostname; both have /api paths."
echo "  See README:"
echo "  \"Reverse Proxy & Device Hostnames\"."
echo
info "MCP server is running!"
echo
echo "  HTTP SSE:         http://localhost:${UB_MCP_PORT}/sse"
echo "  Stdio command:    docker exec -i ub-mcp ub-mcp"
echo
echo "  Create a bearer token in the Settings UI under \"MCP Tokens\" before"
echo "  connecting clients. Settings also displays copy-pasteable client"
echo "  configs for Claude Desktop, Gemini CLI, and similar."
echo
echo "  To reconfigure, just run this script again."
echo "  To view logs: docker logs -f ultrabridge  (or ub-mcp)"
echo
