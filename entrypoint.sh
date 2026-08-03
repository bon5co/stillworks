#!/bin/sh
# Migrate, then serve. Migrations take a database-backed lock, so two replicas
# starting together is safe; a failed migration stops the container rather than
# serving against a schema it does not match.
set -eu

cd /app
./manage migrate
exec ./server "0.0.0.0:${PORT:-8000}"
