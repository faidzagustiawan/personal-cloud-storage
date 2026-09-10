#!/usr/bin/env bash
#
# Build locally, ship to the VPS, restart. Idempotent: safe to run repeatedly.
#
#   ./deploy/deploy.sh faidz@43.157.230.218
#
# The Go binary is cross-compiled here rather than built on the server, which is
# the point of choosing a pure-Go SQLite driver (decision D6): no toolchain, no
# cgo and no build dependencies need to exist on the VPS at all.

set -euo pipefail

HOST="${1:-}"
if [[ -z "$HOST" ]]; then
  echo "usage: $0 user@host" >&2
  exit 1
fi

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

echo "==> Building backend (linux/amd64, version $VERSION)"
(cd "$REPO/backend" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o "$STAGE/cloudapp" .)

echo "==> Building frontend"
(cd "$REPO/frontend" && npm ci --silent && npm run build --silent)

echo "==> Uploading"
ssh "$HOST" 'mkdir -p ~/cloudapp-stage/dist'
scp -q "$STAGE/cloudapp" "$HOST:~/cloudapp-stage/cloudapp"
rsync -a --delete "$REPO/frontend/dist/" "$HOST:~/cloudapp-stage/dist/"
scp -q "$REPO/deploy/cloudapp.service" "$HOST:~/cloudapp-stage/cloudapp.service"
scp -q "$REPO/deploy/nginx.conf" "$HOST:~/cloudapp-stage/nginx.conf"

echo "==> Installing"
ssh "$HOST" 'sudo bash -s' <<'REMOTE'
set -euo pipefail

id -u cloudapp >/dev/null 2>&1 || useradd --system --home /var/lib/cloudapp --shell /usr/sbin/nologin cloudapp
install -d -o cloudapp -g cloudapp -m 750 /var/lib/cloudapp
install -d -m 755 /opt/cloudapp /var/www/cloud
install -d -o root -g root -m 700 /etc/cloudapp

# The environment file holds the storage credentials and is never shipped from
# the developer machine; it is created once, by hand, on the server.
if [[ ! -f /etc/cloudapp/env ]]; then
  echo "!! /etc/cloudapp/env is missing." >&2
  echo "   Copy backend/.env.example there, fill it in, and chmod 600." >&2
  exit 1
fi
chmod 600 /etc/cloudapp/env

# Replacing a running binary in place would fail with "text file busy", so the
# new one is put alongside and moved over atomically.
install -m 755 ~"$SUDO_USER"/cloudapp-stage/cloudapp /opt/cloudapp/cloudapp.new
mv /opt/cloudapp/cloudapp.new /opt/cloudapp/cloudapp

rsync -a --delete ~"$SUDO_USER"/cloudapp-stage/dist/ /var/www/cloud/
chown -R root:root /var/www/cloud

install -m 644 ~"$SUDO_USER"/cloudapp-stage/cloudapp.service /etc/systemd/system/cloudapp.service
install -m 644 ~"$SUDO_USER"/cloudapp-stage/nginx.conf /etc/nginx/sites-available/cloud.faidz.fun
ln -sfn /etc/nginx/sites-available/cloud.faidz.fun /etc/nginx/sites-enabled/cloud.faidz.fun

# ffmpeg is the only external dependency, and only the tier 3 thumbnail worker
# uses it. Its absence degrades that worker rather than breaking the server.
command -v ffmpeg >/dev/null 2>&1 || echo "note: ffmpeg is not installed; server-side thumbnails will be skipped"

systemctl daemon-reload
systemctl enable --now cloudapp
systemctl restart cloudapp

nginx -t
systemctl reload nginx
REMOTE

echo "==> Verifying"
ssh "$HOST" 'curl -fsS --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:8080/api/health'
echo

echo "Deployed $VERSION to $HOST"
