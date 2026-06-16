#!/usr/bin/env bash
#
# Build and deploy countinghouse to a remote host.
#
# Usage:
#   ./deploy/deploy.sh sweeney@garibaldi
#
# Keeps the last 3 versioned binaries in /opt/countinghouse/bin/ and symlinks
# the active one. Restarts the countinghouse service after upload.
# Requires passwordless sudo for systemctl on the remote (see sudoers.sh).
#
# First-time setup: run deploy/install.sh on the target host with sudo.
#
set -euo pipefail

REMOTE="${1:?Usage: $0 user@host}"
SERVICE="countinghouse"
BINARY="countinghouse"
BUILD_DIR="bin"
DEPLOY_DIR="/opt/countinghouse/bin"
HEALTH_URL="http://localhost:8585/healthz"
PUBLIC_HEALTH_URL="https://countinghouse.swee.net/healthz"
KEEP_VERSIONS=3

VERSION=$(date +%Y%m%d-%H%M%S)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
REMOTE_BIN="${BINARY}-${VERSION}"

echo "=== Building $BINARY (linux/amd64) ==="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=${COMMIT}" -o "$BUILD_DIR/$BINARY" ./cmd/countinghouse/
echo "  Built: $BUILD_DIR/$BINARY ($COMMIT)"

echo "=== Uploading to $REMOTE ==="
scp "$BUILD_DIR/$BINARY" "$REMOTE:$DEPLOY_DIR/$REMOTE_BIN"
ssh "$REMOTE" "chmod 755 $DEPLOY_DIR/$REMOTE_BIN"

echo "=== Activating $REMOTE_BIN ==="
ssh "$REMOTE" "ln -sfn $REMOTE_BIN $DEPLOY_DIR/$BINARY"

echo "=== Restarting $SERVICE ==="
ssh "$REMOTE" "sudo systemctl restart $SERVICE"

echo "=== Verifying ==="
sleep 2

if ssh "$REMOTE" "sudo systemctl is-active --quiet $SERVICE"; then
    echo "  ✓ $SERVICE is running"
else
    echo "  ✗ $SERVICE failed to start"
    ssh "$REMOTE" "sudo journalctl -u $SERVICE -n 20 --no-pager"
    exit 1
fi

if ssh "$REMOTE" "curl -fsS --max-time 5 -o /dev/null $HEALTH_URL"; then
    echo "  ✓ $HEALTH_URL healthy (on-host)"
else
    echo "  ✗ health check failed at $HEALTH_URL"
    ssh "$REMOTE" "sudo journalctl -u $SERVICE -n 20 --no-pager"
    exit 1
fi

# Public endpoint: verify the externally-facing path (DNS + TLS + reverse proxy)
# actually serves THIS build. Checked from the dev machine, not the host, so it
# exercises real external reachability. Retries to absorb proxy/restart lag.
echo "=== Verifying public endpoint ==="
PUBLIC_OK=""
for _ in 1 2 3 4 5; do
    BODY=$(curl -fsS --max-time 8 "$PUBLIC_HEALTH_URL" 2>/dev/null) || { sleep 2; continue; }
    if printf '%s' "$BODY" | grep -q "\"version\":\"$COMMIT\""; then PUBLIC_OK=1; break; fi
    sleep 2
done
if [ -n "$PUBLIC_OK" ]; then
    echo "  ✓ $PUBLIC_HEALTH_URL serving $COMMIT"
else
    echo "  ✗ public check failed at $PUBLIC_HEALTH_URL (expected version $COMMIT)"
    echo "    last response: ${BODY:-<none>}"
    echo "    on-host health passed, so this is DNS / TLS / reverse-proxy, not the service."
    exit 1
fi

if ssh "$REMOTE" "sudo journalctl -u $SERVICE -n 20 --no-pager" \
        | grep -qE "invalid_client|identity token fetch failed"; then
    echo ""
    echo "  ✗ CREDENTIAL ERROR: identity auth failed on $REMOTE"
    echo "    Update identity.client_secret in /etc/countinghouse/config.yaml"
    echo "    then: sudo systemctl restart $SERVICE"
    echo ""
    exit 1
fi
echo "  ✓ no credential errors"

echo "=== Cleaning old versions (keeping $KEEP_VERSIONS) ==="
ssh "$REMOTE" "\
  cd $DEPLOY_DIR && \
  ls -t ${BINARY}-* \
    | tail -n +$((KEEP_VERSIONS + 1)) \
    | xargs -r rm --"

echo ""
echo "=== Deployed $VERSION ($COMMIT) ==="
ssh "$REMOTE" "sudo journalctl -u $SERVICE -n 5 --no-pager"
