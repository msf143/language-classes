#!/usr/bin/env bash
# Work on the PROD database (Cloud SQL) from the Codespace.
#
#   scripts/prod-db.sh migrate version   # which migration prod is at
#   scripts/prod-db.sh migrate up        # apply new migrations
#   scripts/prod-db.sh psql              # SQL shell on prod (careful)
#
# The prod database has no allowed networks, so we go through the Cloud SQL Auth
# Proxy: a local program that listens on 127.0.0.1:5433, checks with Google that
# *you* (your gcloud sign-in) hold roles/cloudsql.client, and forwards over TLS.
# The password comes from the same Secret Manager secret the app uses, so it
# never sits in a file. Everything stops when this script exits.
set -euo pipefail
cd "$(dirname "$0")/.."

INSTANCE=hablemos-live:us-east1:hablemos-pg
PORT=5433 # not 5432, so it can't be mistaken for the dev database

# 1. Start the proxy in the background; stop it however this script ends.
cloud-sql-proxy --port "$PORT" --quiet "$INSTANCE" &
PROXY_PID=$!
trap 'kill "$PROXY_PID" 2>/dev/null' EXIT

# Wait (up to 15s) until it accepts connections.
for _ in $(seq 30); do
  pg_isready -q -h 127.0.0.1 -p "$PORT" && break
  sleep 0.5
done

# 2. Read the password out of the app's connection string (keyword=value form).
PGPASSWORD=$(gcloud secrets versions access latest --secret database-url --project hablemos-live \
  | sed -n 's/.*password=\([^ ]*\).*/\1/p')
export PGPASSWORD # psql reads this; keeps the password off the command line

# 3. Run the requested tool. sslmode=disable only covers the hop to the local
#    proxy; the proxy encrypts everything it sends on to Cloud SQL.
case "${1:-}" in
  migrate)
    shift
    migrate -path migrations \
      -database "postgres://app:${PGPASSWORD}@127.0.0.1:${PORT}/app?sslmode=disable" "$@"
    ;;
  psql)
    shift
    psql "host=127.0.0.1 port=$PORT dbname=app user=app sslmode=disable" "$@"
    ;;
  *)
    echo "usage: $0 migrate <args> | psql [args]" >&2
    exit 2
    ;;
esac
