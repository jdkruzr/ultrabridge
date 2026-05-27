#!/usr/bin/env bash
set -euo pipefail

# UltraBridge Rebuild — Rebuild and restart without reconfiguring.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

info() { printf '\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m OK \033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m WARN \033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m FAIL \033[0m %s\n' "$*"; exit 1; }

# Parse arguments: --fresh/-f and --nuke flags
FRESH=false
NUKE=false
YES=false
for arg in "$@"; do
    case "$arg" in
        --fresh|-f) FRESH=true ;;
        --nuke) NUKE=true ;;
        -y|--yes) YES=true ;;
        -h|--help)
            cat <<EOF
Usage: rebuild.sh [OPTIONS]

Rebuild and restart UltraBridge without reconfiguring.
Requires install.sh to have been run first.

Options:
  --fresh, -f   Clear both SQLite databases (notes + tasks) before rebuilding
                (prompts for confirmation unless -y)
  --nuke        Delete ALL UltraBridge data before rebuilding
                (prompts for confirmation unless -y)
  -y, --yes     Skip confirmation prompts (for --fresh and --nuke)
  -h, --help    Show this help message
EOF
            exit 0
            ;;
    esac
done

if [[ ! -f "$SCRIPT_DIR/docker-compose.yml" ]]; then
    fail "No docker-compose.yml found in $SCRIPT_DIR. Run install.sh first."
fi

# --- fresh install option ---

if [[ "$FRESH" == true ]]; then
    DATA_DIR="$SCRIPT_DIR/ultrabridge-data"
    warn "Fresh install requested. This will DELETE:"
    echo "  - Notes database: $DATA_DIR/ultrabridge.db"
    echo "  - Task database:  $DATA_DIR/ultrabridge-tasks.db"
    echo
    if [[ "$YES" != true ]]; then
        printf '  Type "yes" to confirm: '
        read -r confirm
        if [[ "$confirm" != "yes" ]]; then
            fail "Aborted."
        fi
    fi
    info "Stopping container..."
    docker compose -f "$SCRIPT_DIR/docker-compose.yml" stop ultrabridge 2>/dev/null || true
    rm -f "$DATA_DIR/ultrabridge.db" "$DATA_DIR/ultrabridge.db-wal" "$DATA_DIR/ultrabridge.db-shm"
    rm -f "$DATA_DIR/ultrabridge-tasks.db" "$DATA_DIR/ultrabridge-tasks.db-wal" "$DATA_DIR/ultrabridge-tasks.db-shm"
    ok "Databases cleared"
elif [[ "$NUKE" == true ]]; then
    DATA_DIR="$SCRIPT_DIR/ultrabridge-data"
    warn "NUKE requested. This will DELETE EVERYTHING:"
    echo "  - All data: $DATA_DIR/"
    echo
    if [[ "$YES" != true ]]; then
        printf '  Type "nuke" to confirm: '
        read -r confirm
        if [[ "$confirm" != "nuke" ]]; then
            fail "Aborted."
        fi
    fi
    info "Stopping container..."
    docker compose -f "$SCRIPT_DIR/docker-compose.yml" stop ultrabridge 2>/dev/null || true
    rm -rf "$DATA_DIR"
    mkdir -p "$DATA_DIR"
    ok "All data deleted"
fi

info "Building and restarting UltraBridge..."
SERVICES="ultrabridge"
PROFILE_ARGS=()
# ub-mcp is an opt-in sidecar (compose profile "mcp"). Include it only when a
# bearer token is available — from the environment or .env — so a missing token
# never blocks rebuilding the main service. Mint one in Settings → MCP Tokens.
MCP_TOKEN="${UB_MCP_API_TOKEN:-}"
if [[ -z "$MCP_TOKEN" && -f "$SCRIPT_DIR/.env" ]]; then
    MCP_TOKEN="$(grep -E '^UB_MCP_API_TOKEN=' "$SCRIPT_DIR/.env" 2>/dev/null | tail -n1 | cut -d= -f2-)"
fi
if [[ -n "$MCP_TOKEN" ]]; then
    SERVICES="ultrabridge ub-mcp"
    PROFILE_ARGS=(--profile mcp)
    info "MCP token found — including ub-mcp sidecar."
else
    warn "No UB_MCP_API_TOKEN — skipping ub-mcp sidecar (set it in .env to enable)."
fi
docker compose "${PROFILE_ARGS[@]+"${PROFILE_ARGS[@]}"}" -f "$SCRIPT_DIR/docker-compose.yml" up -d --build --force-recreate $SERVICES || fail "Build/restart failed"
ok "Container(s) running"

PORT=$(grep -oP '"\K[0-9]+(?=:8443")' "$SCRIPT_DIR/docker-compose.yml" 2>/dev/null || echo "8443")
HEALTH_URL="http://localhost:${PORT}/health"
HEALTH_TIMEOUT=180
info "Waiting for health check (up to ${HEALTH_TIMEOUT}s)..."
HEALTH_OK=false
for i in $(seq 1 $HEALTH_TIMEOUT); do
    if curl -sf "$HEALTH_URL" >/dev/null 2>&1; then
        HEALTH_OK=true
        break
    fi
    if (( i % 10 == 0 )); then
        echo "  ... still waiting (${i}s)"
    fi
    sleep 1
done

if [[ "$HEALTH_OK" == true ]]; then
    ok "Health check passed (${i}s)"
else
    fail "Health check failed after ${HEALTH_TIMEOUT}s. Run: docker logs ultrabridge"
fi

info "Done!"
