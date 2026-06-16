#!/usr/bin/env bash
#
# First-time bring-up of countinghouse on garibaldi. Run AS ROOT on the host:
#
#   sudo bash bootstrap.sh                                 # prompts for the client secret
#   sudo CH_CLIENT_SECRET='<secret>' bash bootstrap.sh     # non-interactive
#
# Idempotent. It creates the service user, dirs, config, and systemd unit; mints
# a read-only Influx token (only if one isn't already in place); reuses the
# existing id.swee.net "countinghouse" client; and installs the deploy sudoers
# rule. It ENABLES but does not START the service — there's no binary yet. Deploy
# it from the dev machine with `make deploy`, which uploads the binary and starts
# the service.
#
set -euo pipefail

SERVICE=countinghouse
PORT=8585
ORG=swee.net
BUCKET=statehouse
CLIENT_ID=countinghouse
DEPLOY_USER="${SUDO_USER:-sweeney}"

if [ "$(id -u)" -ne 0 ]; then echo "Run as root: sudo bash $0" >&2; exit 1; fi

# Identity client secret — reused from the dev machine's client. Supply it via
# the CH_CLIENT_SECRET env var, or be prompted (hidden input).
SECRET="${CH_CLIENT_SECRET:-}"
if [ -z "$SECRET" ]; then
    printf 'id.swee.net %s client_secret: ' "$CLIENT_ID"
    stty -echo 2>/dev/null || true
    read -r SECRET
    stty echo 2>/dev/null || true
    echo
fi
[ -n "$SECRET" ] || { echo "client_secret is required" >&2; exit 1; }

echo "=== Service user ==="
if ! id "$SERVICE" >/dev/null 2>&1; then
    useradd --system --shell /usr/sbin/nologin --home-dir "/var/lib/$SERVICE" "$SERVICE"
    echo "  created $SERVICE"
else
    echo "  $SERVICE already exists"
fi

echo "=== Directories ==="
# Binary dir is owned by the deploy user so scp from the dev machine works.
mkdir -p /opt/$SERVICE/bin
chown "$DEPLOY_USER:$DEPLOY_USER" /opt/$SERVICE/bin
echo "  /opt/$SERVICE/bin (owner $DEPLOY_USER)"
mkdir -p /var/lib/$SERVICE
chown "$SERVICE:$SERVICE" /var/lib/$SERVICE; chmod 700 /var/lib/$SERVICE
echo "  /var/lib/$SERVICE"
mkdir -p /etc/$SERVICE
chown root:$SERVICE /etc/$SERVICE; chmod 750 /etc/$SERVICE
echo "  /etc/$SERVICE"

echo "=== Influx read token ==="
if [ -s /etc/$SERVICE/influx-token ]; then
    echo "  /etc/$SERVICE/influx-token already present, skipping mint"
else
    BID=$(docker exec influxdb influx bucket list --org "$ORG" --name "$BUCKET" --json \
          | sed -nE 's/.*"id": *"([a-f0-9]+)".*/\1/p' | head -1)
    [ -n "$BID" ] || { echo "  could not resolve bucket '$BUCKET' id" >&2; exit 1; }
    echo "  bucket $BUCKET -> $BID"
    TOK=$(docker exec influxdb influx auth create --org "$ORG" --read-bucket "$BID" \
          --description "${SERVICE}-ro" --json \
          | sed -nE 's/.*"token": *"([^"]+)".*/\1/p' | head -1)
    [ -n "$TOK" ] || { echo "  token mint failed" >&2; exit 1; }
    umask 077
    printf '%s' "$TOK" > /etc/$SERVICE/influx-token
    chown root:$SERVICE /etc/$SERVICE/influx-token; chmod 640 /etc/$SERVICE/influx-token
    echo "  minted read-only token -> /etc/$SERVICE/influx-token"
fi

echo "=== Config ==="
cat > /etc/$SERVICE/config.yaml <<CONFIG
http:
  listen: ":$PORT"
  public_url: "https://$SERVICE.swee.net"

influx:
  url: "http://localhost:8086"
  org: "$ORG"
  bucket: "$BUCKET"
  token_file: "/etc/$SERVICE/influx-token"

identity:
  base_url: "https://id.swee.net"
  client_id: "$CLIENT_ID"
  client_secret: "$SECRET"

remote_config:
  base_url: "https://config.swee.net"

house:
  timezone: "Europe/London"
CONFIG
chown root:$SERVICE /etc/$SERVICE/config.yaml; chmod 640 /etc/$SERVICE/config.yaml
echo "  wrote /etc/$SERVICE/config.yaml (listen :$PORT)"

echo "=== systemd unit ==="
# KEEP IN SYNC with deploy/countinghouse.service
cat > /etc/systemd/system/$SERVICE.service <<'UNIT'
[Unit]
Description=Read-side energy cost/accounting service
After=network-online.target
Wants=network-online.target
Documentation=https://github.com/sweeney/countinghouse

[Service]
Type=simple
User=countinghouse
Group=countinghouse

ExecStart=/opt/countinghouse/bin/countinghouse
WorkingDirectory=/var/lib/countinghouse

Restart=always
RestartSec=5

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/countinghouse
ReadOnlyPaths=/etc/countinghouse
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
MemoryDenyWriteExecute=true
LockPersonality=true

StandardOutput=journal
StandardError=journal
SyslogIdentifier=countinghouse

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable $SERVICE >/dev/null 2>&1 || true
echo "  installed + enabled $SERVICE.service"

echo "=== Deploy sudoers rule ==="
echo "$DEPLOY_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/journalctl" > /etc/sudoers.d/$SERVICE-deploy
chmod 440 /etc/sudoers.d/$SERVICE-deploy
echo "  /etc/sudoers.d/$SERVICE-deploy"

echo ""
echo "=== Bootstrap complete ==="
echo "  Service is enabled but NOT started (no binary yet)."
echo "  From the dev machine:  make deploy   # uploads the binary and starts it on :$PORT"
