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

# Journal position markers, set just before the restart (see journal_since).
# Initialised here so the diagnosis helpers are safe under `set -u` whatever order
# they are reached in.
CURSOR=""
SINCE=""

# --- startup-failure diagnosis ------------------------------------------------
#
# countinghouse refuses to start rather than serving a plausible-looking wrong
# answer: a config naming no devices/floorplan namespace, or a namespace that has
# never been fetched, aborts the boot (see "boot needs truth, running keeps the
# last truth" in README.md). Those refusals are the most likely way a deploy of a
# healthy binary still ends with the service down, and the raw journal buries the
# reason under restart-loop noise.
#
# The binary already writes the explanation, including the exact config block to
# add. The deploy script does not restate it: it renders it, then adds what only
# the deploy knows — whether this will self-heal, and how to get the previous
# build back.

# journal_since prints this boot's log lines only.
#
# It reads from a CURSOR taken just before the restart, not a timestamp.
# `journalctl --since` parses a bare timestamp in the HOST's local timezone
# (systemd.time(7)), so a UTC timestamp on a Europe/London host points an hour
# into the future for eight months of the year: the window comes back empty,
# every classification below misses, and the diagnosis degrades to "not a known
# startup refusal" plus an empty log — silently, and only in summer. A cursor is
# timezone-free and also closes the one-second race between reading the clock and
# the restart landing.
#
# The timestamp form is kept as a fallback for a host whose journalctl does not
# print a cursor, and it uses LOCAL time there for the same reason.
journal_since() {
    if [ -n "$CURSOR" ]; then
        ssh "$REMOTE" "sudo journalctl -u $SERVICE --after-cursor '$CURSOR' --no-pager" 2>/dev/null || true
    else
        ssh "$REMOTE" "sudo journalctl -u $SERVICE --since '$SINCE' --no-pager" 2>/dev/null || true
    fi
}

# render_errors turns slog's one-line JSON back into something readable, so a
# multi-line refusal (which carries the YAML to add) prints as multiple lines
# instead of one wall of \n escapes. Falls back to the raw log without python3.
render_errors() {
    if command -v python3 >/dev/null 2>&1; then
        python3 -c '
import json, sys
seen = set()
for line in sys.stdin:
    i = line.find("{")
    if i < 0:
        continue
    try:
        rec = json.loads(line[i:])
    except ValueError:
        continue
    if rec.get("level") not in ("ERROR", "WARN"):
        continue
    msg = rec.get("msg", "")
    if msg in seen:          # a restart loop repeats the same refusal every 5s
        continue
    seen.add(msg)
    print("    " + msg.replace("\n", "\n    "))
    for k, v in rec.items():
        if k in ("time", "level", "msg"):
            continue
        if isinstance(v, list):
            v = ", ".join(str(x) for x in v)
        # Indent continuation lines too: a refusal carries the YAML block to add
        # in its error value, and it is only readable if it stays aligned.
        print("      %s: %s" % (k, str(v).replace("\n", "\n      ")))
    print()
'
    else
        sed -n 's/.*"msg":"\([^"]*\)".*/    \1/p'
    fi
}

# previous_build names the build this deploy replaced, so the rollback command can
# be printed with a real filename rather than a shell incantation to adapt.
previous_build() {
    ssh "$REMOTE" "cd $DEPLOY_DIR && ls -t ${BINARY}-* 2>/dev/null | sed -n 2p" 2>/dev/null || true
}

fail_with_diagnosis() {
    local headline="$1"
    local log rendered prev caveat
    log=$(journal_since)

    echo "  ✗ $headline"
    echo ""

    # The service's own words first. It writes the diagnosis — including the exact
    # config block to add — so the deploy renders it rather than paraphrasing it.
    rendered=$(printf '%s\n' "$log" | render_errors)
    echo "  ── what the service said ───────────────────────────────────────────"
    if [ -n "$rendered" ]; then
        printf '%s\n' "$rendered"
    else
        echo "    (nothing structured — it did not get as far as logging)"
    fi
    # Command substitution strips trailing newlines, so render_errors' own spacing
    # is lost by the time it lands here. One blank line, added on both paths.
    echo ""

    # The caveat on rolling back differs per cause, and getting it wrong is worse
    # than omitting it: for a missing config key the rollback works but the next
    # deploy fails identically, while for an unreachable dependency the OLD build
    # starts happily and serves the empty snapshots this one refuses to.
    caveat="    (that reverts the binary, not the cause.)"

    echo "  ── what to do ──────────────────────────────────────────────────────"
    if printf '%s' "$log" | grep -q "names no devices_namespace\|names no floorplan_namespace"; then
        # Local config is missing a required key. This never fixes itself.
        echo "    A required key is missing from the host's config. Will not fix itself;"
        echo "    the block above is what to add."
        echo ""
        echo "      ssh $REMOTE 'sudo nano /etc/countinghouse/config.yaml'"
        echo "      ssh $REMOTE 'sudo systemctl restart $SERVICE'"
        caveat="    (reverts the binary, not the cause — the key is still missing.)"
    elif printf '%s' "$log" | grep -q "no snapshot was fetched"; then
        # A namespace was named but never landed. The distinction that matters to
        # whoever is reading this at 3am is whether it will fix ITSELF: an
        # unreachable dependency does (Restart=always, every 5s), a wrong secret or
        # a wrong namespace name never does. Classified by the underlying error, not
        # by which step reported it — "identity token fetch failed" covers both a
        # rejected credential and an identity service that is simply down.
        if printf '%s' "$log" | grep -q "invalid_client\|unauthorized"; then
            echo "    Identity rejected these credentials, so nothing could be fetched."
            echo "    Will not fix itself."
            echo ""
            echo "      ssh $REMOTE 'sudo grep client_ /etc/countinghouse/config.yaml'"
            echo "      # fix identity.client_secret, then:"
            echo "      ssh $REMOTE 'sudo systemctl restart $SERVICE'"
        elif printf '%s' "$log" | grep -q "unexpected status 404"; then
            echo "    A namespace was named but the config service returns 404: wrong name,"
            echo "    or not published yet. Will not fix itself."
            echo ""
            echo "      ssh $REMOTE 'sudo grep _namespace /etc/countinghouse/config.yaml'"
        elif printf '%s' "$log" | grep -qE "connection refused|no such host|i/o timeout|deadline exceeded|TLS handshake"; then
            echo "    Identity or config.swee.net was unreachable, so a namespace has no"
            echo "    snapshot. Nothing on the host is broken — the unit retries every 5s"
            echo "    and comes up on its own once the dependency answers."
            echo ""
            echo "      ssh $REMOTE 'systemctl status $SERVICE'   # watch it recover"
        else
            echo "    A namespace was named but never fetched, for a reason not seen before"
            echo "    — the log above is the whole story."
        fi
        caveat="    (wrong move here: an older build starts without a snapshot and serves
     the empty data this one refuses. Fix the fetch, or wait for it.)"
    else
        echo "    Not a known startup refusal. Full log for this boot:"
        echo ""
        printf '%s\n' "$log" | tail -n 20 | sed 's/^/      /'
    fi

    prev=$(previous_build)
    if [ -n "$prev" ]; then
        echo ""
        echo "    If you need the previous build serving right now:"
        echo "      ssh $REMOTE 'ln -sfn $prev $DEPLOY_DIR/$BINARY && sudo systemctl restart $SERVICE'"
        printf '%s\n' "$caveat"
    fi
    echo ""
    exit 1
}
# ------------------------------------------------------------------------------

echo "=== Building $BINARY (linux/amd64) ==="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=${COMMIT}" -o "$BUILD_DIR/$BINARY" ./cmd/countinghouse/
echo "  Built: $BUILD_DIR/$BINARY ($COMMIT)"

echo "=== Uploading to $REMOTE ==="
scp "$BUILD_DIR/$BINARY" "$REMOTE:$DEPLOY_DIR/$REMOTE_BIN"
ssh "$REMOTE" "chmod 755 $DEPLOY_DIR/$REMOTE_BIN"

echo "=== Activating $REMOTE_BIN ==="
ssh "$REMOTE" "ln -sfn $REMOTE_BIN $DEPLOY_DIR/$BINARY"

# Mark the journal position so the diagnosis reads only THIS boot's logs — without
# it a stale refusal from an earlier attempt would be reported as the cause of this
# one. A cursor is preferred over a timestamp; see journal_since for why. Both are
# captured so the fallback is available if this journalctl prints no cursor. The
# timestamp is LOCAL time, because that is how journalctl reads an unqualified one.
#
# `-n 1`, not `-n 0`: --show-cursor is documented as printing the cursor AFTER the
# entries shown, so a version that takes that literally prints none when none are
# shown — leaving CURSOR empty and the timestamp fallback quietly doing the work
# every time. systemd 257 on the current host prints a cursor either way (checked),
# so this is portability rather than a live bug. The sed keeps only the cursor
# line, so the one log entry `-n 1` prints is discarded and nothing else changes.
CURSOR=$(ssh "$REMOTE" "sudo journalctl -u $SERVICE -n 1 --show-cursor --no-pager 2>/dev/null | sed -n 's/^-- cursor: //p'" 2>/dev/null || true)
SINCE=$(ssh "$REMOTE" "date +'%Y-%m-%d %H:%M:%S'")
if [ -z "$CURSOR" ]; then
    # Say so rather than degrade quietly: the fallback is correct, but a preferred
    # path that is silently dead is how the timezone bug survived in the first place.
    echo "  note: no journal cursor available (unit has no entries, or journalctl does"
    echo "        not print one) — a failed start will be diagnosed from a timestamp window"
fi

echo "=== Restarting $SERVICE ==="
ssh "$REMOTE" "sudo systemctl restart $SERVICE"

echo "=== Verifying ==="
sleep 2

if ssh "$REMOTE" "sudo systemctl is-active --quiet $SERVICE"; then
    echo "  ✓ $SERVICE is running"
else
    fail_with_diagnosis "$SERVICE failed to start"
fi

if ssh "$REMOTE" "curl -fsS --max-time 5 -o /dev/null $HEALTH_URL"; then
    echo "  ✓ $HEALTH_URL healthy (on-host)"
else
    fail_with_diagnosis "health check failed at $HEALTH_URL"
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

if journal_since | grep -qE "invalid_client|identity token fetch failed"; then
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
