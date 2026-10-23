#!/bin/sh
#
# The container's entry point: the two processes, started from outside.
#
# The API comes first, because the site's first request is the API's, and a site
# that started earlier would merely take longer to answer. Neither process is
# published: the site is the only one that listens on anything but loopback.
#
set -e

APP_DIR="${APP_DIR:-/app}"
API_PORT="${PORT:-8080}"
WEB_PORT="${WEB_PORT:-4321}"
HOST="${HOST:-0.0.0.0}"
CONTENT_ROOT="${CONTENT_ROOT:-/app/content}"

# An empty content volume has no Markdown templates, and the effect registry
# reads them from CONTENT_ROOT at request time — so a fresh volume would render
# every article without a single callout style. The image carries the project's
# own templates; they are copied in once, on the first start.
if [ ! -d "$CONTENT_ROOT/system" ] && [ -d /app/pristine/system ]; then
  echo "==> seeding $CONTENT_ROOT/system from the image"
  mkdir -p "$CONTENT_ROOT"
  cp -R /app/pristine/system "$CONTENT_ROOT/system"
fi

"$APP_DIR/blogcms-server" &
api=$!

# Wait for the API. The probe is node, because node is certainly in this image
# and curl is not. Both the URL and the process are watched: a process that
# died on startup leaves the port free, and a probe against a different server
# that happens to own the port would report success for a process that is not
# running at all.
i=0
while [ "$i" -lt 100 ]; do
  kill -0 "$api" 2>/dev/null || { echo "==> the API exited before it became ready" >&2; exit 1; }
  if node -e "fetch('http://127.0.0.1:${API_PORT}/api/v1/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))" >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1)); sleep 0.3
done
[ "$i" -lt 100 ] || { echo "==> the API did not become ready" >&2; kill "$api"; exit 1; }

# The site gets its own PORT. Both processes read the same variable name, and
# letting them inherit one value means one of them binds the other's port —
# which is how the site comes up with no web server at all.
env PORT="$WEB_PORT" HOST="$HOST" node "$APP_DIR/astro/dist/server/entry.mjs" &
web=$!

# A container stop is a SIGTERM: both processes are asked to leave, and the
# shell waits for the site so the container does not exit before it has.
trap 'kill $api $web 2>/dev/null || true' INT TERM
wait $web
